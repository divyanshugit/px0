package main

// Remote mode: `px0 user@host:path` runs px0 on another machine over ssh and
// forwards its port back to the local browser.
//
// Nothing on the remote side is exposed to the network. px0 there binds
// loopback, ssh -L carries the port to loopback here, and the browser opens
// http://127.0.0.1:<port>. Because the page is reached by IP address, harness
// edits keep working, and the harness runs on the machine where the code is.
//
// The remote px0 is tied to the ssh session: the launcher script watches the
// session's stdin and kills px0 when it reaches EOF, which happens when the
// local px0 exits, is interrupted, or the connection drops. When px0 exits on
// its own the launcher exits with it, so a failure ends the session too.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// remoteTarget is a parsed `[user@]host:path` or `ssh://[user@]host[:port]/path`.
type remoteTarget struct {
	Dest string // [user@]host, as ssh accepts it
	Port string // ssh port, "" for the default
	Path string // remote path; "" means the login directory
}

// parseRemoteTarget recognises scp-style and ssh:// targets. A path that exists
// locally is never remote, so a directory that happens to contain a colon still
// opens. `file.go:12` for a missing file is left alone too, so the usual error
// for a bad local target is reported.
func parseRemoteTarget(target string) (remoteTarget, bool) {
	if target == "" {
		return remoteTarget{}, false
	}
	if _, err := os.Stat(target); err == nil {
		return remoteTarget{}, false
	}
	if strings.HasPrefix(target, "ssh://") {
		u, err := url.Parse(target)
		if err != nil || u.Hostname() == "" {
			return remoteTarget{}, false
		}
		dest := u.Hostname()
		if u.User != nil && u.User.Username() != "" {
			dest = u.User.Username() + "@" + dest
		}
		return remoteTarget{Dest: dest, Port: u.Port(), Path: u.Path}, true
	}
	if filepath.VolumeName(target) != "" {
		return remoteTarget{}, false
	}
	i := strings.Index(target, ":")
	if i <= 0 {
		return remoteTarget{}, false
	}
	host, path := target[:i], target[i+1:]
	if !validSSHDest(host) {
		return remoteTarget{}, false
	}
	if path != "" && isDigits(path) {
		return remoteTarget{}, false
	}
	return remoteTarget{Dest: host, Path: path}, true
}

// validSSHDest accepts `host`, `user@host`, and ssh config aliases: letters,
// digits, dots, hyphens, underscores, and a single @.
func validSSHDest(s string) bool {
	if s == "" || strings.Count(s, "@") > 1 || strings.HasPrefix(s, "-") {
		return false
	}
	user, host, hasUser := strings.Cut(s, "@")
	if hasUser {
		if user == "" || host == "" || strings.HasPrefix(host, "-") {
			return false
		}
	} else {
		host = s
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_', r == '@':
		default:
			return false
		}
	}
	return !isDigits(host)
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// remoteOptions carries what the local px0 was asked for and what to forward.
type remoteOptions struct {
	LocalPort   int      // local port to forward from; walks forward when busy
	NoOpen      bool     // do not open the browser
	Passthrough []string // px0 flags repeated on the remote (-no-lsp, -agent, ...)
	Stdout      io.Writer
	Stderr      io.Writer
	Stdin       io.Reader                  // used for the install prompt
	OpenBrowser func(url string)           // nil uses openBrowser
	Ask         func(question string) bool // nil prompts on Stdin when it is a terminal
}

// remoteBinCandidates are the places px0 is looked for on the remote. A
// non-interactive ssh shell often lacks ~/.local/bin on PATH, which is where
// install.sh and our own installer put it.
var remoteBinCandidates = []string{
	"px0",
	`"$HOME/.local/bin/px0"`,
	`"$HOME/bin/px0"`,
	"/usr/local/bin/px0",
	"/opt/homebrew/bin/px0",
}

const remoteMissingMarker = "px0-remote: missing"

// remoteScript builds the POSIX sh script run on the remote. It finds px0,
// reports `px0-remote: missing <uname -s> <uname -m>` with exit 3 when it
// isn't there, and otherwise runs px0 tied to the session's stdin.
func remoteScript(path string, port int, passthrough []string) string {
	var b strings.Builder
	b.WriteString(`PX0=""; for c in ` + strings.Join(remoteBinCandidates, " ") + `; do if command -v "$c" >/dev/null 2>&1; then PX0=$(command -v "$c"); break; fi; done; `)
	b.WriteString(`if [ -z "$PX0" ]; then echo "` + remoteMissingMarker + ` $(uname -s) $(uname -m)"; exit 3; fi; `)
	b.WriteString(`"$PX0" -no-open -no-color -port ` + strconv.Itoa(port))
	for _, f := range passthrough {
		b.WriteString(" " + shellQuote(f))
	}
	if p := remotePathArg(path); p != "" {
		b.WriteString(" -- " + p)
	}
	// A background list gets /dev/null as stdin in a non-interactive shell, so
	// the session's stdin is duplicated onto fd 3 first and the watcher reads
	// that. px0 itself was started before the dup and holds no copy.
	b.WriteString(` & p=$!; exec 3<&0; ( cat <&3 >/dev/null; kill "$p" 2>/dev/null ) >/dev/null 2>&1 & wait "$p"`)
	return b.String()
}

// remotePathArg quotes a remote path for sh, letting a leading ~ expand to the
// remote home directory. An empty path means the login directory and yields "".
func remotePathArg(path string) string {
	switch {
	case path == "" || path == ".":
		return ""
	case path == "~":
		return `"$HOME"`
	case strings.HasPrefix(path, "~/"):
		rest := strings.TrimPrefix(path, "~/")
		if rest == "" {
			return `"$HOME"`
		}
		return `"$HOME"/` + shellQuote(rest)
	}
	return shellQuote(path)
}

// remoteInstallScript downloads px0 on the remote with install.sh, using
// whichever of curl or wget is present.
const remoteInstallScript = `if command -v curl >/dev/null 2>&1; then curl -fsSL https://px0.ai/install.sh | sh; elif command -v wget >/dev/null 2>&1; then wget -qO- https://px0.ai/install.sh | sh; else echo "px0: neither curl nor wget on the remote" >&2; exit 1; fi`

// remoteCopyScript receives a binary on stdin and installs it as ~/.local/bin/px0.
const remoteCopyScript = `d="$HOME/.local/bin"; mkdir -p "$d" && cat > "$d/.px0.tmp" && chmod 755 "$d/.px0.tmp" && mv -f "$d/.px0.tmp" "$d/px0" && "$d/px0" -version`

// unameToGo maps `uname -s` and `uname -m` to GOOS and GOARCH the way
// install.sh does. Unknown values return "".
func unameToGo(sys, machine string) (goos, goarch string) {
	switch strings.ToLower(sys) {
	case "linux":
		goos = "linux"
	case "darwin":
		goos = "darwin"
	case "freebsd":
		goos = "freebsd"
	case "openbsd":
		goos = "openbsd"
	case "netbsd":
		goos = "netbsd"
	}
	switch strings.ToLower(machine) {
	case "x86_64", "amd64":
		goarch = "amd64"
	case "aarch64", "arm64":
		goarch = "arm64"
	case "armv7l", "armv6l", "arm":
		goarch = "arm"
	case "i386", "i686":
		goarch = "386"
	case "riscv64":
		goarch = "riscv64"
	}
	return goos, goarch
}

var urlPattern = regexp.MustCompile(`https?://[^\s]+`)

// rewriteViewerURL points a URL printed by the remote px0 at the local end of
// the forward, keeping its path and query.
func rewriteViewerURL(remote string, localPort int) (string, error) {
	u, err := url.Parse(remote)
	if err != nil {
		return "", err
	}
	u.Host = net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort))
	return u.String(), nil
}

// remoteLine classifies a line of the remote px0's stdout so the local side
// can re-narrate it. kind is one of "heading", "kv", "url", "hint", "blank",
// "status", "bullet" or "text"; a and b carry the parsed pieces.
func remoteLine(line string) (kind, a, b string) {
	trimmed := strings.TrimSpace(line)
	switch {
	case trimmed == "":
		return "blank", "", ""
	case strings.HasPrefix(trimmed, "px0 ") && !strings.Contains(trimmed, ":"):
		return "heading", strings.TrimPrefix(trimmed, "px0 "), ""
	case trimmed == "ctrl-c to stop":
		return "hint", "", ""
	}
	if strings.HasPrefix(line, "  ") {
		if label, value, ok := strings.Cut(trimmed, ":"); ok && !strings.ContainsAny(label, " \t") {
			value = strings.TrimSpace(value)
			if label == "url" {
				return "url", urlPattern.FindString(value), ""
			}
			return "kv", label, value
		}
	}
	for _, m := range [][2]string{{"[OK]", "ok"}, {"[FAIL]", "err"}, {"[WARN]", "warn"}, {"[INFO]", "info"}, {">", "step"}} {
		if strings.HasPrefix(trimmed, m[0]+" ") {
			return "status", m[1], strings.TrimSpace(strings.TrimPrefix(trimmed, m[0]))
		}
	}
	if rest, ok := strings.CutPrefix(trimmed, "· "); ok {
		return "bullet", strings.TrimSpace(rest), ""
	}
	return "text", trimmed, ""
}

// errRemotePortTaken reports that the remote px0 walked to another port
// because the one we forwarded was busy; the caller retries with a new one.
var errRemotePortTaken = errors.New("remote port taken")

// errRemoteMissing carries the remote platform when px0 is not installed there.
type errRemoteMissing struct{ sys, machine string }

func (e errRemoteMissing) Error() string {
	return fmt.Sprintf("px0 is not installed on the remote (%s %s)", e.sys, e.machine)
}

// runRemote is the whole remote session: it starts px0 over ssh with a port
// forward, opens the browser on the local end, relays the remote narration,
// and returns when the session ends or ctx is cancelled. When px0 is missing
// on the remote it offers to install it and starts again.
func runRemote(ctx context.Context, rt remoteTarget, opts remoteOptions) error {
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	if opts.Stdin == nil {
		opts.Stdin = os.Stdin
	}
	if opts.OpenBrowser == nil {
		opts.OpenBrowser = openBrowser
	}
	if opts.Ask == nil {
		opts.Ask = func(q string) bool { return askYesNo(q, opts.Stdin, opts.Stdout) }
	}
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		return errors.New("remote targets need the ssh client on PATH")
	}

	uiHeading("px0 "+version, nil, opts.Stdout)
	uiKV("remote", uiAccent(rt.Dest, opts.Stdout), 11, opts.Stdout)

	installed := false
	for attempt := 0; attempt < 4; attempt++ {
		err := runRemoteSession(ctx, sshBin, rt, opts)
		var missing errRemoteMissing
		switch {
		case err == nil:
			return nil
		case errors.Is(err, errRemotePortTaken):
			uiStatus("warn", "remote port was busy, retrying", "", 0, opts.Stdout)
			continue
		case errors.As(err, &missing) && !installed:
			if err := installRemote(ctx, sshBin, rt, missing, opts); err != nil {
				return err
			}
			installed = true
			continue
		default:
			return err
		}
	}
	return errors.New("could not start px0 on the remote after several attempts")
}

// runRemoteSession runs one ssh session and returns when it ends.
func runRemoteSession(ctx context.Context, sshBin string, rt remoteTarget, opts remoteOptions) error {
	ln, addr, err := listen("127.0.0.1", opts.LocalPort)
	if err != nil {
		return err
	}
	_, portStr, _ := net.SplitHostPort(addr)
	localPort, _ := strconv.Atoi(portStr)
	ln.Close() // ssh binds it a moment later

	remotePort := 20000 + rand.IntN(40000)
	args := sshArgs(rt, localPort, remotePort)
	args = append(args, "sh -c "+shellQuote(remoteScript(rt.Path, remotePort, opts.Passthrough)))

	cmd := exec.Command(sshBin, args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ssh: %w", err)
	}
	stderrDone := make(chan struct{})
	go func() {
		relayStderr(stderr, opts.Stderr)
		close(stderrDone)
	}()

	// Ending the session: closing stdin makes the remote launcher kill px0 and
	// exit, which closes the connection. If ssh lingers, it is killed.
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			stdin.Close()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				_ = cmd.Process.Kill()
			}
		case <-done:
		}
	}()

	var sessionErr error
	gotURL := false
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		kind, a, b := remoteLine(sc.Text())
		switch kind {
		case "blank", "hint":
		case "heading":
			if a != version {
				uiKV("remote px0", a+"  "+uiDim("(local "+version+")", opts.Stdout), 11, opts.Stdout)
			}
		case "kv":
			uiKV(a, b, 11, opts.Stdout)
		case "url":
			if a == "" {
				sessionErr = errors.New("could not read the url printed by the remote px0")
				stdin.Close()
				continue
			}
			u, err := url.Parse(a)
			if err != nil || u.Port() != strconv.Itoa(remotePort) {
				sessionErr = errRemotePortTaken
				stdin.Close()
				continue
			}
			local, _ := rewriteViewerURL(a, localPort)
			gotURL = true
			uiKV("url", uiAccent(local, opts.Stdout), 11, opts.Stdout)
			uiHint("ctrl-c to stop", opts.Stdout)
			if !opts.NoOpen {
				go opts.OpenBrowser(local)
			}
		case "status":
			uiStatus(a, b, "", 0, opts.Stdout)
		case "bullet":
			uiBullet(a, opts.Stdout)
		case "text":
			if strings.HasPrefix(a, remoteMissingMarker) {
				f := strings.Fields(strings.TrimPrefix(a, remoteMissingMarker))
				m := errRemoteMissing{}
				if len(f) > 0 {
					m.sys = f[0]
				}
				if len(f) > 1 {
					m.machine = f[1]
				}
				sessionErr = m
				continue
			}
			uiBullet(a, opts.Stdout)
		}
	}
	<-stderrDone
	waitErr := cmd.Wait()
	close(done)

	if sessionErr != nil {
		return sessionErr
	}
	if waitErr != nil {
		// Ctrl-C reaches ssh too, and it may die before our own handler has
		// cancelled ctx. Give that a moment before calling it a failure.
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(300 * time.Millisecond):
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	if !gotURL {
		if waitErr != nil {
			return fmt.Errorf("remote px0 did not start (%v); see the messages above", waitErr)
		}
		return errors.New("remote px0 exited before it published a url")
	}
	if waitErr != nil {
		return fmt.Errorf("ssh session ended: %v", waitErr)
	}
	return nil
}

// relayStderr copies ssh's stderr through, dropping the two lines that only
// restate what the local side already narrates: ssh's own note when Ctrl-C
// kills it, and the remote px0's shutdown line.
func relayStderr(r io.Reader, w io.Writer) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		t := strings.TrimSpace(line)
		if killedBySignal.MatchString(t) || t == "[INFO] px0 stopped" {
			continue
		}
		fmt.Fprintln(w, line)
	}
}

var killedBySignal = regexp.MustCompile(`^Killed by signal \d+\.$`)

// sshArgs are the ssh flags for one session: a loopback-to-loopback forward
// that fails loudly when the local port cannot be bound, keepalives so a dead
// link is noticed, and the destination.
func sshArgs(rt remoteTarget, localPort, remotePort int) []string {
	args := []string{
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		"-L", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", localPort, remotePort),
	}
	if rt.Port != "" {
		args = append(args, "-p", rt.Port)
	}
	return append(args, "--", rt.Dest)
}

// installRemote puts px0 on the remote after asking. When the remote runs the
// same OS and architecture, the local binary is streamed over ssh, so no
// network access is needed there and versions match exactly. Otherwise the
// remote fetches its own build with install.sh.
func installRemote(ctx context.Context, sshBin string, rt remoteTarget, m errRemoteMissing, opts remoteOptions) error {
	goos, goarch := unameToGo(m.sys, m.machine)
	platform := strings.TrimSpace(m.sys + " " + m.machine)
	if goos != "" && goarch != "" {
		platform = goos + "/" + goarch
	}
	if !opts.Ask(fmt.Sprintf("px0 is not installed on %s (%s). Install it to ~/.local/bin there?", rt.Dest, platform)) {
		return fmt.Errorf("px0 is not installed on %s; install it with: curl -fsSL https://px0.ai/install.sh | sh", rt.Dest)
	}

	var args []string
	if rt.Port != "" {
		args = append(args, "-p", rt.Port)
	}
	args = append(args, "--", rt.Dest)

	if goos == runtime.GOOS && goarch == runtime.GOARCH {
		exe, err := os.Executable()
		if err == nil {
			exe, err = filepath.EvalSymlinks(exe)
		}
		if err != nil {
			return fmt.Errorf("locate local px0 binary: %w", err)
		}
		f, err := os.Open(exe)
		if err != nil {
			return err
		}
		defer f.Close()
		uiStatus("step", "copying local px0 "+version+" to "+rt.Dest, "", 0, opts.Stdout)
		cmd := exec.CommandContext(ctx, sshBin, append(args, "sh -c "+shellQuote(remoteCopyScript))...)
		cmd.Stdin = f
		cmd.Stdout = opts.Stdout
		cmd.Stderr = opts.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("copy px0 to %s: %w", rt.Dest, err)
		}
		return nil
	}

	uiStatus("step", "installing px0 on "+rt.Dest+" with install.sh", "", 0, opts.Stdout)
	cmd := exec.CommandContext(ctx, sshBin, append(args, "sh -c "+shellQuote(remoteInstallScript))...)
	cmd.Stdout = opts.Stdout
	cmd.Stderr = opts.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("install px0 on %s: %w", rt.Dest, err)
	}
	return nil
}

// askYesNo prints a question and reads one line. Without a terminal on stdin
// there is nobody to answer, so the answer is no.
func askYesNo(question string, in io.Reader, out io.Writer) bool {
	if f, ok := in.(*os.File); ok && !isTTY(f) {
		return false
	}
	fmt.Fprintf(out, "%s %s ", uiGlyph("step", out), question+" [Y/n]")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(out)
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true
	}
	return false
}
