package link

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

// wire records every byte the panel's server reads and writes: what
// Cloudflare would see past its TLS.
type wire struct {
	net.Listener
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *wire) Accept() (net.Conn, error) {
	c, err := w.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &tapped{c, w}, nil
}

func (w *wire) seen() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

type tapped struct {
	net.Conn
	w *wire
}

func (t *tapped) Read(p []byte) (int, error) {
	n, err := t.Conn.Read(p)
	t.w.mu.Lock()
	t.w.buf.Write(p[:n])
	t.w.mu.Unlock()
	return n, err
}

func (t *tapped) Write(p []byte) (int, error) {
	t.w.mu.Lock()
	t.w.buf.Write(p)
	t.w.mu.Unlock()
	return t.Conn.Write(p)
}

// panel is a Server that admits nodes with the token's secret once, then
// by their key; each session it gets goes to sessions.
func panel(t *testing.T) (url string, key ed25519.PrivateKey, token Token, sessions chan *yamux.Session, w *wire) {
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	token, err = NewToken(key)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	known := map[string]bool{}
	used := false
	sessions = make(chan *yamux.Session, 4)
	sv := &Server{Key: key, Version: "v1", Admit: func(node ed25519.PublicKey, join, version string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case known[string(node)]:
		case join == token.Secret && !used:
			used, known[string(node)] = true, true
		default:
			return "", errors.New("unknown node")
		}
		return "n1", nil
	}, Serve: func(name, version string, s *yamux.Session) {
		sessions <- s
		<-s.CloseChan()
	}}
	srv := httptest.NewUnstartedServer(sv)
	w = &wire{Listener: srv.Listener}
	srv.Listener = w
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL, key, token, sessions, w
}

func TestNodeJoinsAndTalksBothWays(t *testing.T) {
	url, _, token, sessions, w := panel(t)
	ctx := context.Background()
	nodeKey, _ := NewKey()
	sess, version, err := Dial(ctx, url, nodeKey, token.PanelKey, token.Secret, "v1")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if version != "v1" {
		t.Errorf("panel version %q", version)
	}
	panelSide := <-sessions

	// Streams both ways; what goes over them doesn't show on the wire.
	go func() {
		s, err := sess.Accept()
		if err != nil {
			return
		}
		defer s.Close()
		_, _ = io.Copy(s, s) // echo
	}()
	stream, err := panelSide.Open()
	if err != nil {
		t.Fatal(err)
	}
	const secretVar = "DATABASE_URL=postgres://u:hunter2@db/main"
	if _, err := io.WriteString(stream, secretVar); err != nil {
		t.Fatal(err)
	}
	back := make([]byte, len(secretVar))
	if _, err := io.ReadFull(stream, back); err != nil || string(back) != secretVar {
		t.Fatalf("echo %q, %v", back, err)
	}
	stream.Close()
	if !strings.Contains(w.seen(), "Upgrade: websocket") {
		t.Error("no WebSocket on the wire")
	}
	if strings.Contains(w.seen(), "hunter2") || strings.Contains(w.seen(), token.Secret) {
		t.Error("the link's content shows on the wire")
	}

	// The token is used up; the node's key admits it again.
	if _, _, err := Dial(ctx, url, mustKey(t), token.PanelKey, token.Secret, "v1"); err == nil {
		t.Error("a used token admitted another node")
	}
	again, _, err := Dial(ctx, url, nodeKey, token.PanelKey, "", "v1")
	if err != nil {
		t.Fatalf("the joined node can't connect again: %v", err)
	}
	again.Close()
}

func TestNodeChecksThePanelsKey(t *testing.T) {
	url, _, token, _, _ := panel(t)
	other, _ := NewKey()
	_, _, err := Dial(context.Background(), url, mustKey(t), Public(other), token.Secret, "v1")
	if err == nil || !strings.Contains(err.Error(), "panel's key") {
		t.Errorf("connected to a panel with another key: %v", err)
	}
}

func TestTokenRoundTrip(t *testing.T) {
	key, _ := NewKey()
	tok, _ := NewToken(key)
	got, err := ParseToken(tok.String())
	if err != nil || got.Secret != tok.Secret || !got.PanelKey.Equal(tok.PanelKey) {
		t.Errorf("parsed %+v, %v", got, err)
	}
	if _, err := ParseToken("not-a-token"); err == nil {
		t.Error("parsed garbage")
	}
}

func TestSessionKeepsAliveAndEnds(t *testing.T) {
	url, _, token, sessions, _ := panel(t)
	sess, _, err := Dial(context.Background(), url, mustKey(t), token.PanelKey, token.Secret, "v1")
	if err != nil {
		t.Fatal(err)
	}
	panelSide := <-sessions
	if _, err := panelSide.Ping(); err != nil {
		t.Errorf("ping: %v", err)
	}
	sess.Close()
	select {
	case <-panelSide.CloseChan():
	case <-time.After(5 * time.Second):
		t.Error("the panel didn't see the node go")
	}
}

func mustKey(t *testing.T) ed25519.PrivateKey {
	k, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}
