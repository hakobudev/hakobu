package ops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/x0ryz/hakobu/internal/link"
	"github.com/x0ryz/hakobu/internal/node"
	"github.com/x0ryz/hakobu/internal/node/nodetest"
	"github.com/x0ryz/hakobu/internal/store"
)

// joinServer makes server name joined and, with f, connected: its calls
// reach f over the link's protocol.
func joinServer(t *testing.T, s *store.Store, name string, f *nodetest.Fake) {
	t.Helper()
	if err := s.CreateNode(ctx(), store.CreateNodeParams{Name: name}); err != nil {
		t.Fatal(err)
	}
	n, _ := s.GetNodeByName(ctx(), name)
	key, _ := link.NewKey()
	if err := s.JoinNode(ctx(), store.JoinNodeParams{PublicKey: fmt.Sprintf("%x", link.Public(key)), ID: n.ID}); err != nil {
		t.Fatal(err)
	}
	if f == nil {
		return
	}
	r, closeLink, err := nodetest.Link(f)
	if err != nil {
		t.Fatal(err)
	}
	links.mu.Lock()
	links.m[name] = &linked{remote: r.(*node.Remote), session: nil}
	links.mu.Unlock()
	t.Cleanup(func() {
		links.mu.Lock()
		delete(links.m, name)
		links.mu.Unlock()
		closeLink()
	})
}

func TestProjectOnAnotherServer(t *testing.T) {
	cf := newFakeAccounts(t)
	s, own, _ := accountsStore(t)
	here, far := nodetest.New(), nodetest.New()
	old := local
	local = nodetest.Linked(t, here)
	t.Cleanup(func() { local = old })
	joinServer(t, s, "far", far)

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(SetProjectServer(s, "own", "far"))
	must(s.CreateApp(ctx(), store.CreateAppParams{ProjectID: own.ID, Name: "web", BuildStrategy: "dockerfile"}))
	far.Images["web"] = map[node.Image]string{node.Next: "build-1"}
	if err := SetProjectServer(s, "own", ""); err == nil {
		t.Error("a project with an app moved to another server")
	}

	// It deploys on its server, not the panel's.
	must(rollOut(s, webApp(t, s), node.Next, &strings.Builder{}))
	if far.Containers["web-green"] != "running" || here.Called("StartCandidate") {
		t.Errorf("far %v, here called StartCandidate: %v", far.Containers, here.Called("StartCandidate"))
	}

	// Its domain gets a tunnel of the panel's account on that server, which
	// doesn't reach the panel.
	must(SetAppDomain(s, webApp(t, s), "web.panel.com"))
	if cf.records["web.panel.com"] != "t-new-1.cfargotunnel.com" {
		t.Errorf("records %v", cf.records)
	}
	tun := far.Tunnels[node.TunnelName("")]
	if tun.Token != "token-of-t-new-1" || tun.PanelSocket != "" || fmt.Sprint(tun.Projects) != "[own]" {
		t.Errorf("tunnel on far: %+v", tun)
	}
	must(SyncTunnel(s))
	if got := fmt.Sprint(cf.ingress["t-new-1"]); got != "[{web.panel.com  http://web.hakobu:8080} {  http_status:404}]" {
		t.Errorf("far's tunnel routes %s", got)
	}
	if got := fmt.Sprint(cf.ingress["t-panel"]); strings.Contains(got, "web.panel.com") {
		t.Errorf("the panel's tunnel routes far's app: %s", got)
	}

	// When it connects again, it's brought up to date: its tunnels, its
	// projects' networks, slots and proxies.
	far.Fail("RunTunnel", nil)
	c, _ := links.get("far")
	gone, cancel := context.WithCancel(context.Background())
	cancel() // stop watching at once
	before := len(far.Calls())
	serverConnected(s, "far", c.remote, gone)
	calls := strings.Join(far.Calls()[before:], " ")
	for _, want := range []string{"RunTunnel", "EnsureProjectNetworks", "Reconcile", "EnsureProxy"} {
		if !strings.Contains(calls, want) {
			t.Errorf("on connecting, far got %s, want %s too", calls, want)
		}
	}

	// A server running a project can't be removed; once it's gone, its
	// tunnels go too.
	if err := RemoveServer(s, "far"); err == nil || !strings.Contains(err.Error(), "own") {
		t.Errorf("removed a server running a project: %v", err)
	}
	must(DeleteProject(s, "own"))
	must(RemoveServer(s, "far"))
	if fmt.Sprint(cf.deleted) != "[t-new-1]" {
		t.Errorf("tunnels deleted in Cloudflare: %v", cf.deleted)
	}
	if rows, _ := s.ListServerTunnels(ctx()); len(rows) != 0 {
		t.Errorf("server tunnels left: %+v", rows)
	}
}

// A server that isn't connected fails its projects' jobs at once, and
// nothing in the records changes.
func TestServerAway(t *testing.T) {
	newFakeAccounts(t)
	s, own, _ := accountsStore(t)
	joinServer(t, s, "away", nil)
	if err := SetProjectServer(s, "own", "away"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateApp(ctx(), store.CreateAppParams{ProjectID: own.ID, Name: "web", BuildStrategy: "dockerfile"}); err != nil {
		t.Fatal(err)
	}
	before := webApp(t, s)
	err := rollOut(s, before, node.Next, &strings.Builder{})
	if !errors.Is(err, node.ErrUnreachable) {
		t.Errorf("rollOut on a server away: %v", err)
	}
	if after := webApp(t, s); after.ActiveSlot != before.ActiveSlot || after.LivePort != before.LivePort {
		t.Errorf("record changed: %s:%d, was %s:%d", after.ActiveSlot, after.LivePort, before.ActiveSlot, before.LivePort)
	}
	if err := SetProjectServer(s, "own", "nowhere"); err == nil {
		t.Error("moved to an unknown server")
	}
}
