package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/hakobudev/hakobu/internal/secret"
	"github.com/hakobudev/hakobu/internal/store/teldb"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/hakobudev/hakobu/internal/config"
	"github.com/hakobudev/hakobu/internal/link"
	"github.com/hakobudev/hakobu/internal/node"
	"github.com/hakobudev/hakobu/internal/node/nodetest"
	"github.com/hakobudev/hakobu/internal/store"
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
	n := waitServer(t, s, "client-a")
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
	clientA := serverNamed(s, "client-a")
	if err := RemoveServer(s, clientA.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sess.CloseChan():
	case <-time.After(5 * time.Second):
		t.Error("the removed server's link stayed up")
	}
	if _, err := serverNode(clientA); !errors.Is(err, node.ErrUnreachable) {
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
	if err := s.SetNodeJoin(ctx(), store.SetNodeJoinParams{JoinSecretHash: secretHash(tok.Secret), JoinExpires: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), ID: serverNamed(s, "late").ID}); err != nil {
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

func waitServer(t *testing.T, s *store.Store, name string) node.Node {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if n, err := serverNode(serverNamed(s, name)); err == nil {
			return n
		}
	}
	t.Fatalf("server %s never connected", name)
	return nil
}

// A server learns the DSN keys of its own apps over its link, and only
// theirs; what they send is kept on it, not on the panel.
func TestServerKeepsItsAppsTelemetry(t *testing.T) {
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
	token, _ := AddServer(s, 0, "far")
	tok, _ := link.ParseToken(token)
	key, _ := link.NewKey()
	sess, _, err := link.Dial(context.Background(), srv.URL, key, tok.PanelKey, tok.Secret, "v1")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	far := nodetest.New()
	farTel, err := store.OpenNodeTelemetry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	far.Tel = farTel.Q
	go func() { _ = node.Serve(sess, far, sess.Open) }()
	waitServer(t, s, "far")

	for _, p := range []string{"there", "here"} {
		server := ""
		if p == "there" {
			server = "far"
		}
		if _, err := CreateProjectOn(s, 0, p, server, ""); err != nil {
			t.Fatal(err)
		}
		pr, _ := getProject(s, p)
		if err := createApp(s, store.CreateAppParams{ProjectID: pr.ID, Name: p + "-app", BuildStrategy: "dockerfile"}); err != nil {
			t.Fatal(err)
		}
		if err := s.SetAppSentryKey(ctx(), store.SetAppSentryKeyParams{ID: appID(t, s, p+"-app"), SentryKey: secret.String(p + "-key")}); err != nil {
			t.Fatal(err)
		}
	}
	there, _ := getApp(s, "there-app")
	here, _ := getApp(s, "here-app")
	toPanel := &http.Client{Transport: &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) { return sess.Open() }}}
	ask := func(id int64) (IngestApp, int) {
		resp, err := toPanel.Get(fmt.Sprintf("http://panel/ingest/apps/%d", id))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var a IngestApp
		_ = json.NewDecoder(resp.Body).Decode(&a)
		return a, resp.StatusCode
	}
	if a, code := ask(there.ID); code != http.StatusOK || a != (IngestApp{there.ID, "there-key"}) {
		t.Errorf("its own app: %d %+v", code, a)
	}
	if a, code := ask(here.ID); code != http.StatusNotFound || a.Key != "" {
		t.Errorf("another server's app: %d %+v", code, a)
	}

	envelope := []byte(`{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}
{"type":"event"}
{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc","level":"error","message":"boom"}
`)
	for _, app := range []store.App{there, here} {
		if err := StoreEnvelope(s, app.ID, envelope); err != nil {
			t.Fatal(err)
		}
	}
	if events, err := AppEvents(s, there, true, 10); err != nil || len(events) != 1 {
		t.Errorf("there-app's events: %+v, %v", events, err)
	}
	// Its issues are grouped and resolved on its server, over the link.
	if issues, err := AppIssues(s, there, false, 10); err != nil || len(issues) != 1 {
		t.Errorf("there-app's issues: %+v, %v", issues, err)
	} else if err := ResolveIssue(s, there, issues[0].ID); err != nil {
		t.Errorf("resolving there-app's issue: %v", err)
	} else if open, _ := AppIssues(s, there, false, 10); len(open) != 0 {
		t.Errorf("there-app's issue is still open: %+v", open)
	}
	if err := ResolveIssue(s, there, 9999); !errors.Is(err, ErrNoIssue) {
		t.Errorf("resolving an issue that isn't there: %v", err)
	}
	if kept, _ := s.Tel.ListTelemetryEvents(ctx(), teldb.ListTelemetryEventsParams{AppID: there.ID, Limit: 10}); len(kept) != 0 {
		t.Errorf("the panel kept there-app's events: %+v", kept)
	}
	if kept, _ := farTel.Q.ListTelemetryEvents(ctx(), teldb.ListTelemetryEventsParams{AppID: here.ID, Limit: 10}); len(kept) != 0 {
		t.Errorf("the server kept here-app's events: %+v", kept)
	}
	if events, _ := AppEvents(s, here, true, 10); len(events) != 1 {
		t.Errorf("here-app's events: %+v", events)
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
	h, err := LinkServer(s, "v1")
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
	waitServer(t, s, "follower")
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
