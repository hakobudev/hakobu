package client

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// An API on loopback is reached through the system's ssh (the person's
// keys and ~/.ssh/config), one ssh -W a connection, with the key server
// create left on the server, which may only reach the API.

// SSHCommand is the ssh to run: HAKOBU_SSH's, as GIT_SSH is git's.
var SSHCommand = sshCommand()

func sshCommand() string {
	if c := os.Getenv("HAKOBU_SSH"); c != "" {
		return c
	}
	return "ssh"
}

type sshTransport struct {
	target, key, addr string
	http              *http.Transport

	mu      sync.Mutex
	lastErr string // what ssh last said as it failed
}

var (
	transportsMu sync.Mutex
	transports   = map[string]*sshTransport{}
)

// transportFor is one transport a server, kept for the process: the MCP
// server reuses its idle connections instead of an ssh a call.
func transportFor(target, key, addr string) *sshTransport {
	transportsMu.Lock()
	defer transportsMu.Unlock()
	id := target + "\x00" + key + "\x00" + addr
	if t, ok := transports[id]; ok {
		return t
	}
	t := &sshTransport{target: target, key: key, addr: addr}
	t.http = &http.Transport{
		DialContext:     func(ctx context.Context, _, _ string) (net.Conn, error) { return t.dial() },
		MaxIdleConns:    4,
		IdleConnTimeout: 2 * time.Minute,
	}
	transports[id] = t
	return t
}

// SSHArgs are ssh's arguments to reach addr on target with key: never
// asking, as it runs under a command or an agent.
func SSHArgs(target, key string, extra ...string) []string {
	args := []string{"-o", "BatchMode=yes", "-o", "ServerAliveInterval=15", "-o", "ExitOnForwardFailure=yes"}
	if key != "" {
		args = append(args, "-i", key, "-o", "IdentitiesOnly=yes")
	}
	return append(append(args, extra...), target)
}

func (t *sshTransport) dial() (net.Conn, error) {
	cmd := exec.Command(SSHCommand, SSHArgs(t.target, t.key, "-W", t.addr)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	ours, theirs := net.Pipe()
	go func() {
		_, _ = io.Copy(stdin, theirs)
		_ = stdin.Close()
	}()
	go func() {
		_, _ = io.Copy(theirs, stdout)
		if cmd.Wait() != nil && strings.TrimSpace(stderr.String()) != "" {
			t.mu.Lock()
			t.lastErr = lastLine(stderr.String())
			t.mu.Unlock()
		}
		_ = theirs.Close()
	}()
	return &sshConn{Conn: ours, cmd: cmd}, nil
}

// why is what ssh said as it last failed, for an error.
func (t *sshTransport) why() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.lastErr == "" {
		return " (through ssh " + t.target + ")"
	}
	return " (ssh " + t.target + ": " + t.lastErr + ")"
}

type sshConn struct {
	net.Conn
	cmd  *exec.Cmd
	once sync.Once
}

func (c *sshConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
	})
	return err
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
