package hive

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Hive-RSI-Lab/hive-dispatch/internal/wire"
)

// Link is one session with a runner: an SSH session
// whose stdin and stdout carry frames. The Hive dials; the runner listens on
// nothing of its own.
type Link struct {
	*wire.Conn
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *os.File
	stderr *lockedBuf
	done   chan struct{}
}

// RemoteCommand is what an ssh:// address runs on the other machine when its
// key isn't held to a forced command (it should be: see docs/README.md).
const RemoteCommand = "hive-dispatch runner serve"

// testLinks lets the tests reach a runner on this machine through a local
// command instead of ssh, by its address. Only _test.go files and the
// score's rehearsal (link, in-process) fill it: a runner in runners.json is
// always ssh://, so ssh is the only command a real runner's link runs.
var testLinks = map[string][]string{}

// safePath is a hive-dispatch path on the runner, sent as the remote command.
var safePath = regexp.MustCompile(`^/[A-Za-z0-9/._-]+$`)

// safeHost and safeUser keep an address's host and user from reading as
// one of ssh's options (`ssh://u@-oProxyCommand=id`): no leading '-', and
// only what a host name, an IP address or a login name holds.
var (
	safeHost = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.:-]*$`)
	safeUser = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)
	safePort = regexp.MustCompile(`^[0-9]{1,5}$`)
)

// Argv is the command that opens a link to r: ssh, to an ssh:// address.
func (h Home) Argv(r Runner) ([]string, error) {
	if argv, ok := testLinks[r.Address]; ok {
		return argv, nil
	}
	u, err := url.Parse(r.Address)
	if err != nil || u.Scheme != "ssh" || u.Hostname() == "" || u.User.Username() == "" {
		return nil, fmt.Errorf("runner %s: address %q is not ssh://user@host[:port]", r.Name, r.Address)
	}
	port := u.Port()
	if port == "" {
		port = "22"
	}
	if !safeHost.MatchString(u.Hostname()) || !safeUser.MatchString(u.User.Username()) || !safePort.MatchString(port) {
		return nil, fmt.Errorf("runner %s: address %q: the user and host may hold only letters, digits, '.', '_', ':' and '-', and may not start with '-'", r.Name, r.Address)
	}
	remote := RemoteCommand
	if u.Path != "" && u.Path != "/" {
		if !safePath.MatchString(u.Path) {
			return nil, fmt.Errorf("runner %s: the path in %q may hold only letters, digits, '/', '.', '_' and '-'", r.Name, r.Address)
		}
		remote = u.Path + " runner serve"
	}
	// Only the module's own key and its own pinned host keys: no ssh config,
	// no agent, no proxy command, no prompts, no forwarding of any kind; and
	// "--", so nothing after it reads as an option.
	return []string{"ssh", "-F", "/dev/null", "-T", "-o", "ProxyCommand=none", "-o", "ProxyJump=none",
		"-i", h.Key(), "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none",
		"-o", "UserKnownHostsFile=" + h.KnownHosts(), "-o", "GlobalKnownHostsFile=/dev/null", "-o", "StrictHostKeyChecking=yes",
		"-o", "HostKeyAlias=" + r.Name,
		"-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=4",
		"-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "ClearAllForwardings=yes", "-o", "PermitLocalCommand=no",
		"-p", port, "-l", u.User.Username(), "--", u.Hostname(), remote}, nil
}

// Dial opens a link to r.
func (h Home) Dial(r Runner) (*Link, error) {
	argv, err := h.Argv(r)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	// Nothing of the caller's environment goes along: the core starts a turn
	// with the daemon's own.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + string(h), "LANG=C.UTF-8"}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	// Not StdoutPipe: Wait closes that as soon as the command exits, which
	// can lose the last frames still unread.
	out, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = w
	l := &Link{cmd: cmd, in: in, out: out, stderr: &lockedBuf{}, done: make(chan struct{})}
	cmd.Stderr = l.stderr
	err = cmd.Start()
	w.Close()
	if err != nil {
		out.Close()
		return nil, fmt.Errorf("runner %s: %w", r.Name, err)
	}
	go func() { cmd.Wait(); close(l.done) }()
	l.Conn = wire.New(out, in)
	return l, nil
}

// Why adds what the link's command said on stderr to err.
func (l *Link) Why(err error) error {
	if s := strings.TrimSpace(l.stderr.String()); s != "" {
		return fmt.Errorf("%w (%s)", err, clip(s, 400))
	}
	return err
}

// Close ends the session.
func (l *Link) Close() {
	l.in.Close()
	select {
	case <-l.done:
	case <-time.After(5 * time.Second):
		l.cmd.Process.Kill()
		<-l.done
	}
	l.out.Close()
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.b.Len() > 64<<10 {
		return len(p), nil
	}
	return b.b.Write(p)
}

func (b *lockedBuf) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
