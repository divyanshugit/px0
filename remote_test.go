package main

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseRemoteTarget(t *testing.T) {
	dir := t.TempDir()
	withColon := filepath.Join(dir, "a:b")
	if err := os.MkdirAll(withColon, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		in   string
		want remoteTarget
		ok   bool
	}{
		{"vm:~/work/repo", remoteTarget{Dest: "vm", Path: "~/work/repo"}, true},
		{"deploy@10.0.0.7:/srv/app", remoteTarget{Dest: "deploy@10.0.0.7", Path: "/srv/app"}, true},
		{"build-box.internal:", remoteTarget{Dest: "build-box.internal", Path: ""}, true},
		{"vm:repo/main.go:42", remoteTarget{Dest: "vm", Path: "repo/main.go:42"}, true},
		{"ssh://me@vm:2222/srv/app", remoteTarget{Dest: "me@vm", Port: "2222", Path: "/srv/app"}, true},
		{"ssh://vm/srv", remoteTarget{Dest: "vm", Path: "/srv"}, true},
		// Local things that must stay local.
		{".", remoteTarget{}, false},
		{"", remoteTarget{}, false},
		{"main.go:12", remoteTarget{}, false},            // missing file with a line number
		{"localhost:8080", remoteTarget{}, false},        // digits after the colon
		{withColon, remoteTarget{}, false},               // exists locally
		{"src/pkg:thing", remoteTarget{}, false},         // slash in the host part
		{"-oProxyCommand=x:path", remoteTarget{}, false}, // would be read as an ssh flag
		{"@vm:path", remoteTarget{}, false},
		{"a@b@c:path", remoteTarget{}, false},
		{":path", remoteTarget{}, false},
	}
	for _, c := range cases {
		got, ok := parseRemoteTarget(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("parseRemoteTarget(%q) = %+v, %v; want %+v, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestRemoteScript(t *testing.T) {
	s := remoteScript("~/work/my repo", 41234, []string{"-no-lsp", "-agent", "claude --model x"})
	for _, want := range []string{
		`-port 41234`,
		`-no-open -no-color`,
		` -no-lsp -agent 'claude --model x'`,
		` -- "$HOME"/'work/my repo'`,
		remoteMissingMarker + ` $(uname -s) $(uname -m)"; exit 3`,
		`exec 3<&0; ( cat <&3 >/dev/null; kill "$p"`,
		`wait "$p"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script missing %q:\n%s", want, s)
		}
	}
	if s := remoteScript("", 1, nil); strings.Contains(s, " -- ") {
		t.Errorf("empty path should not pass an argument: %s", s)
	}
	if s := remoteScript("/srv/app", 1, nil); !strings.Contains(s, " -- /srv/app &") {
		t.Errorf("absolute path not passed plainly: %s", s)
	}
	if got := remotePathArg("~"); got != `"$HOME"` {
		t.Errorf("remotePathArg(~) = %s", got)
	}
	if got := remotePathArg("it's"); got != `'it'\''s'` {
		t.Errorf("remotePathArg quote = %s", got)
	}
}

func TestUnameToGo(t *testing.T) {
	cases := []struct{ sys, machine, goos, goarch string }{
		{"Linux", "x86_64", "linux", "amd64"},
		{"Linux", "aarch64", "linux", "arm64"},
		{"Darwin", "arm64", "darwin", "arm64"},
		{"FreeBSD", "amd64", "freebsd", "amd64"},
		{"Linux", "armv7l", "linux", "arm"},
		{"Linux", "riscv64", "linux", "riscv64"},
		{"Plan9", "mips", "", ""},
	}
	for _, c := range cases {
		goos, goarch := unameToGo(c.sys, c.machine)
		if goos != c.goos || goarch != c.goarch {
			t.Errorf("unameToGo(%s, %s) = %s/%s, want %s/%s", c.sys, c.machine, goos, goarch, c.goos, c.goarch)
		}
	}
}

func TestRewriteViewerURL(t *testing.T) {
	got, err := rewriteViewerURL("http://127.0.0.1:41234/?path=main.go&line=3", 7777)
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://127.0.0.1:7777/?path=main.go&line=3" {
		t.Errorf("got %s", got)
	}
}

func TestRemoteLine(t *testing.T) {
	cases := []struct{ in, kind, a, b string }{
		{"", "blank", "", ""},
		{"px0 0.1.5", "heading", "0.1.5", ""},
		{"  workspace:  /srv/app", "kv", "workspace", "/srv/app"},
		{"  url:        http://127.0.0.1:41234/?path=a.go", "url", "http://127.0.0.1:41234/?path=a.go", ""},
		{"ctrl-c to stop", "hint", "", ""},
		{"[OK] indexed 12 files  3ms", "status", "ok", "indexed 12 files  3ms"},
		{"[WARN] something", "status", "warn", "something"},
		{"  · language servers: gopls", "bullet", "language servers: gopls", ""},
		{remoteMissingMarker + " Linux x86_64", "text", remoteMissingMarker + " Linux x86_64", ""},
	}
	for _, c := range cases {
		kind, a, b := remoteLine(c.in)
		if kind != c.kind || a != c.a || b != c.b {
			t.Errorf("remoteLine(%q) = %q, %q, %q; want %q, %q, %q", c.in, kind, a, b, c.kind, c.a, c.b)
		}
	}
}

// fakeSSH puts a stand-in ssh and a stand-in px0 onto PATH. The ssh logs its
// arguments and runs the remote command locally through sh, so the launcher
// script is exercised for real: the fake px0 prints a banner with the port it
// was given and waits to be killed. With missingFirst the first session
// reports that px0 is absent and an install run (a remote command mentioning
// install.sh) succeeds.
func fakeSSH(t *testing.T, missingFirst bool) (logPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake ssh needs sh")
	}
	dir := t.TempDir()
	logPath = filepath.Join(dir, "log")
	state := filepath.Join(dir, "state")
	missing := "0"
	if missingFirst {
		missing = "1"
	}
	ssh := `#!/bin/sh
printf '%s\n' "$*" >> "` + logPath + `"
while [ $# -gt 0 ]; do
  case "$1" in
    -L|-o|-p) shift ;;
    --) shift; break ;;
  esac
  shift
done
shift # destination
cmd="$*"
case "$cmd" in
  *install.sh*) echo "installed"; exit 0 ;;
esac
if [ "` + missing + `" = "1" ] && [ ! -f "` + state + `" ]; then
  : > "` + state + `"
  echo "` + remoteMissingMarker + ` FreeBSD riscv64"
  exit 3
fi
sh -c "$cmd"
echo "Killed by signal 2." >&2
exit 130
`
	px0 := `#!/bin/sh
port=""
while [ $# -gt 0 ]; do case "$1" in -port) port="$2"; shift ;; esac; shift; done
echo
echo "px0 9.9.9"
echo "  workspace:  /srv/app"
echo "  url:        http://127.0.0.1:$port/?path=main.go"
echo
echo "ctrl-c to stop"
echo "[OK] indexed 3 files  1ms"
trap 'echo "[INFO] px0 stopped" >&2; exit 130' TERM INT
while :; do sleep 1; done
`
	for name, body := range map[string]string{"ssh": ssh, "px0": px0} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// syncBuffer is a bytes.Buffer safe for the relay goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestRunRemoteForwardsAndStops(t *testing.T) {
	logPath := fakeSSH(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var out, errOut syncBuffer
	opened := make(chan string, 1)
	opts := remoteOptions{
		LocalPort:   0,
		Passthrough: []string{"-no-lsp"},
		Stdout:      &out,
		Stderr:      &errOut,
		OpenBrowser: func(u string) {
			opened <- u
			cancel() // the user is done; Ctrl-C
		},
	}
	err := runRemote(ctx, remoteTarget{Dest: "me@vm", Path: "~/repo"}, opts)
	if err != nil {
		t.Fatalf("runRemote: %v\nstdout:\n%s\nstderr:\n%s", err, out.String(), errOut.String())
	}

	var opened1 string
	select {
	case opened1 = <-opened:
	default:
		t.Fatal("browser was never opened")
	}
	u, err := url.Parse(opened1)
	if err != nil || u.Hostname() != "127.0.0.1" || u.RawQuery != "path=main.go" {
		t.Fatalf("browser opened on %q", opened1)
	}

	log, _ := os.ReadFile(logPath)
	args := strings.TrimSpace(string(log))
	fwd := "-L 127.0.0.1:" + u.Port() + ":127.0.0.1:"
	if !strings.Contains(args, fwd) {
		t.Errorf("ssh not given the forward %q: %s", fwd, args)
	}
	if !strings.Contains(args, "-o ExitOnForwardFailure=yes") || !strings.Contains(args, " -- me@vm sh -c ") {
		t.Errorf("unexpected ssh arguments: %s", args)
	}
	// The remote px0 is asked for the same port that was forwarded.
	rest := args[strings.Index(args, fwd)+len(fwd):]
	rport := rest[:strings.IndexAny(rest, " ")]
	if _, err := strconv.Atoi(rport); err != nil || !strings.Contains(args, "-port "+rport+" -no-lsp -- \"$HOME\"/repo") {
		t.Errorf("remote px0 not started on forwarded port %s with passthrough flags: %s", rport, args)
	}

	got := out.String()
	for _, want := range []string{"remote:", "me@vm", "remote px0:", "9.9.9", "workspace:", "/srv/app", "url:", opened1, "indexed 3 files"} {
		if !strings.Contains(got, want) {
			t.Errorf("stdout missing %q:\n%s", want, got)
		}
	}
	if e := errOut.String(); strings.Contains(e, "Killed by signal") || strings.Contains(e, "px0 stopped") {
		t.Errorf("ssh's signal note and the remote shutdown line should be dropped: %s", e)
	}
}

func TestRunRemoteInstallsWhenMissing(t *testing.T) {
	logPath := fakeSSH(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var out, errOut syncBuffer
	asked := ""
	opts := remoteOptions{
		Stdout: &out,
		Stderr: &errOut,
		Ask: func(q string) bool {
			asked = q
			return true
		},
		OpenBrowser: func(string) { cancel() },
	}
	if err := runRemote(ctx, remoteTarget{Dest: "vm", Port: "2222"}, opts); err != nil {
		t.Fatalf("runRemote: %v\n%s\n%s", err, out.String(), errOut.String())
	}
	if !strings.Contains(asked, "vm") || !strings.Contains(asked, "freebsd/riscv64") {
		t.Errorf("install prompt = %q", asked)
	}
	log, _ := os.ReadFile(logPath)
	runs := strings.Split(strings.TrimSpace(string(log)), "\n")
	if len(runs) != 3 {
		t.Fatalf("expected probe, install and start sessions, got %d:\n%s", len(runs), log)
	}
	if !strings.Contains(runs[1], "install.sh") || !strings.Contains(runs[1], "-p 2222 -- vm") {
		t.Errorf("second session should install over port 2222: %s", runs[1])
	}
	if !strings.Contains(runs[2], "-p 2222") || strings.Contains(runs[2], "install.sh") {
		t.Errorf("third session should start px0: %s", runs[2])
	}
	if !strings.Contains(out.String(), "installed") {
		t.Errorf("installer output not relayed:\n%s", out.String())
	}
}

func TestRunRemoteRefusesWithoutInstallConsent(t *testing.T) {
	fakeSSH(t, true)
	var out, errOut syncBuffer
	opts := remoteOptions{
		Stdout: &out,
		Stderr: &errOut,
		Ask:    func(string) bool { return false },
	}
	err := runRemote(context.Background(), remoteTarget{Dest: "vm"}, opts)
	if err == nil || !strings.Contains(err.Error(), "install.sh") {
		t.Fatalf("expected an error pointing at install.sh, got %v", err)
	}
}
