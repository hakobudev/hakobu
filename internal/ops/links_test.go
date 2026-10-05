package ops

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/x0ryz/hakobu/internal/config"
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
	h, err := LinkServer(s, "v1", http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	token, err := AddServer(s, 0, "client-a")
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
	sess, p, err := link.Dial(ctx, srv.URL, key, tok.PanelKey, tok.Secret, "v1")
	if err != nil || p.Version != "v1" {
		t.Fatalf("join: %v (%+v)", err, p)
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
	if _, err := AddServer(s, 0, "client-a"); err == nil {
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
	token, err := AddServer(s, 0, "late")
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

// A server passes on its apps' envelopes over its link, and only theirs.
func TestServerSendsItsAppsEnvelopes(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	ingest := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.PathValue("app_id")+" "+r.Header.Get("X-Sentry-Auth"))
	})
	h, err := LinkServer(s, "v1", ingest)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	token, _ := AddServer(s, 0, "far")
	tok, _ := link.ParseToken(token)
	key, _ := link.NewKey()
	sess, _, err := link.Dial(context.Background(), srv.URL, key, tok.PanelKey, tok.Secret, "v1")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	go func() { _ = node.Serve(sess, nodetest.New(), sess.Open) }()
	waitServer(t, "far")

	for _, p := range []string{"there", "here"} {
		server := ""
		if p == "there" {
			server = "far"
		}
		if err := CreateProjectOn(s, 0, p, server, ""); err != nil {
			t.Fatal(err)
		}
		pr, _ := s.GetProject(ctx(), p)
		if err := s.CreateApp(ctx(), store.CreateAppParams{ProjectID: pr.ID, Name: p + "-app", BuildStrategy: "dockerfile"}); err != nil {
			t.Fatal(err)
		}
	}
	there, _ := s.GetApp(ctx(), "there-app")
	here, _ := s.GetApp(ctx(), "here-app")
	toPanel := &http.Client{Transport: &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) { return sess.Open() }}}
	post := func(id int64) int {
		req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://panel/api/%d/envelope/", id), strings.NewReader("{}"))
		req.Header.Set("X-Sentry-Auth", "Sentry sentry_key=k")
		resp, err := toPanel.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(there.ID); code != http.StatusOK {
		t.Errorf("its own app's envelope: %d", code)
	}
	if code := post(here.ID); code != http.StatusForbidden {
		t.Errorf("another server's app's envelope: %d", code)
	}
	if fmt.Sprint(got) != fmt.Sprintf("[%d Sentry sentry_key=k]", there.ID) {
		t.Errorf("ingested %v", got)
	}
}

// A server learns the panel's address when it connects, and its new one
// over the link when the panel moves.
func TestServersFollowThePanel(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := config.SetPublicHost("panel.example.com"); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := LinkServer(s, "v1", http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	// What the panel does for the server writes to its database until
	// the server is gone.
	t.Cleanup(linkWork.Wait)
	srv := httptest.NewServer(h)
	defer srv.Close()
	token, err := AddServer(s, 0, "follower")
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := link.ParseToken(token)
	key, _ := link.NewKey()
	sess, p, err := link.Dial(context.Background(), srv.URL, key, tok.PanelKey, tok.Secret, "v1")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if p.URL != "https://panel.example.com" {
		t.Errorf("told the panel is at %q", p.URL)
	}

	told := make(chan string, 1)
	node.PanelMoved = func(url string) error { told <- url; return nil }
	t.Cleanup(func() { node.PanelMoved = nil })
	go func() { _ = node.Serve(sess, nodetest.New(), sess.Open) }()
	waitServer(t, "follower")
	tellServersPanelURL("https://panel.example.net")
	select {
	case url := <-told:
		if url != "https://panel.example.net" {
			t.Errorf("told %q", url)
		}
	case <-time.After(5 * time.Second):
		t.Error("the server wasn't told the panel moved")
	}
}
