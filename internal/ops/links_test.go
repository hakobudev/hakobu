package ops

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/x0ryz/hakobu/internal/link"
	"github.com/x0ryz/hakobu/internal/node"
	"github.com/x0ryz/hakobu/internal/node/nodetest"
	"github.com/x0ryz/hakobu/internal/store"
)

// A server joins with its token over a real link and then answers the
// panel's calls; once removed, its key no longer admits it.
func TestServerJoinsAndServes(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := LinkServer(s, "v1")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	token, err := AddServer(s, "client-a")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := link.ParseToken(token)
	if err != nil {
		t.Fatal(err)
	}
	if servers, _ := Servers(s); len(servers) != 1 || servers[0].Joined || servers[0].Connected {
		t.Errorf("before joining: %+v", servers)
	}

	key, _ := link.NewKey()
	ctx := context.Background()
	sess, version, err := link.Dial(ctx, srv.URL, key, tok.PanelKey, tok.Secret, "v1")
	if err != nil || version != "v1" {
		t.Fatalf("join: %v (%s)", err, version)
	}
	f := nodetest.New()
	f.Containers["web-blue"] = "running"
	go func() { _ = node.Serve(sess, f, sess.Open) }()
	n := waitServer(t, "client-a")
	if st := n.State(ctx, "web-blue"); st.Status != "running" {
		t.Errorf("state over the link: %+v", st)
	}
	servers, _ := Servers(s)
	if len(servers) != 1 || !servers[0].Joined || !servers[0].Connected || servers[0].Version != "v1" {
		t.Errorf("joined: %+v", servers)
	}

	// The token is spent: another key can't join with it.
	other, _ := link.NewKey()
	if _, _, err := link.Dial(ctx, srv.URL, other, tok.PanelKey, tok.Secret, "v1"); err == nil {
		t.Error("a spent token admitted another server")
	}
	if _, err := AddServer(s, "client-a"); err == nil {
		t.Error("a joined server got a new token")
	}

	// Removed, it's dropped and its key admits it no more.
	if err := RemoveServer(s, "client-a"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sess.CloseChan():
	case <-time.After(5 * time.Second):
		t.Error("the removed server's link stayed up")
	}
	if _, err := serverNode("client-a"); !errors.Is(err, node.ErrUnreachable) {
		t.Errorf("a removed server is reachable: %v", err)
	}
	if _, _, err := link.Dial(ctx, srv.URL, key, tok.PanelKey, "", "v1"); err == nil {
		t.Error("a removed server's key still admits it")
	}
}

func TestJoinTokenExpires(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	token, err := AddServer(s, "late")
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := link.ParseToken(token)
	if err := s.SetNodeJoin(ctx(), store.SetNodeJoinParams{JoinSecretHash: secretHash(tok.Secret), JoinExpires: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), Name: "late"}); err != nil {
		t.Fatal(err)
	}
	key, _ := link.NewKey()
	if _, err := admit(s, link.Public(key), tok.Secret, "v1"); err == nil {
		t.Error("an expired token admitted a server")
	}
	if _, err := admit(s, link.Public(key), "", "v1"); err == nil {
		t.Error("an unknown key was admitted without a token")
	}
}

func waitServer(t *testing.T, name string) node.Node {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if n, err := serverNode(name); err == nil {
			return n
		}
	}
	t.Fatalf("server %s never connected", name)
	return nil
}
