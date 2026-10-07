package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hakobudev/hakobu/internal/ops"
)

// A node knows its apps' DSN keys from the panel, asked once a minute at
// most, and still knows them while the panel is away; an app the panel
// says isn't the node's is forgotten.
func TestNodeAsksThePanelForItsApps(t *testing.T) {
	var asked atomic.Int32
	mine := true
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		if r.URL.Path != "/ingest/apps/7" || !mine {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(ops.IngestApp{ID: 7, Key: "k"})
	}))
	defer panel.Close()
	// The link is a client whose requests reach the panel, whatever the host.
	linked := &http.Client{Transport: rewriteHost{panel.URL}}
	var toPanel atomic.Pointer[http.Client]
	toPanel.Store(linked)
	apps := &panelApps{toPanel: &toPanel, known: map[int64]knownApp{}}
	ctx := context.Background()

	if a, ok := apps.find(ctx, 7); !ok || a != (ingestApp{7, "k"}) {
		t.Fatalf("find: %+v %v", a, ok)
	}
	apps.find(ctx, 7)
	if asked.Load() != 1 {
		t.Errorf("asked the panel %d times within a minute", asked.Load())
	}
	if _, ok := apps.find(ctx, 8); ok {
		t.Error("found an app the panel doesn't give")
	}

	// The panel away: what it told still holds, however old.
	toPanel.Store(nil)
	apps.known[7] = knownApp{ingestApp{7, "k"}, time.Now().Add(-time.Hour)}
	if _, ok := apps.find(ctx, 7); !ok {
		t.Error("forgot the app while the panel is away")
	}

	// Back, and the app moved elsewhere.
	toPanel.Store(linked)
	mine = false
	if _, ok := apps.find(ctx, 7); ok {
		t.Error("kept an app that went to another server")
	}
}

type rewriteHost struct{ base string }

func (r rewriteHost) RoundTrip(req *http.Request) (*http.Response, error) {
	u := r.base + req.URL.Path
	out, err := http.NewRequestWithContext(req.Context(), req.Method, u, req.Body)
	if err != nil {
		return nil, err
	}
	out.Header = req.Header
	if !strings.HasPrefix(u, "http") {
		return nil, http.ErrNotSupported
	}
	return http.DefaultTransport.RoundTrip(out)
}

// Just started, a node gets envelopes before its link to the panel is up:
// it waits a moment for the link rather than refuse apps it doesn't know yet.
func TestNodeWaitsForTheLinkToKnowAnApp(t *testing.T) {
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(ops.IngestApp{ID: 7, Key: "k"})
	}))
	defer panel.Close()
	var toPanel atomic.Pointer[http.Client]
	apps := &panelApps{toPanel: &toPanel, known: map[int64]knownApp{}}
	go func() {
		time.Sleep(300 * time.Millisecond)
		toPanel.Store(&http.Client{Transport: rewriteHost{panel.URL}})
	}()
	if a, ok := apps.find(context.Background(), 7); !ok || a != (ingestApp{7, "k"}) {
		t.Errorf("find before the link: %+v %v", a, ok)
	}
}
