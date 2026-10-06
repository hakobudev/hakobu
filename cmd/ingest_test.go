package cmd

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/x0ryz/hakobu/internal/ops"
	"github.com/x0ryz/hakobu/internal/store"
	"github.com/x0ryz/hakobu/internal/store/teldb"
)

// What the Python SDK sent (internal/ingest/testdata) is stored: the error
// with the trace it happened in, the request that failed kept whole and
// both requests counted by route.
func TestIngestTraces(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateProject(ctx, store.CreateProjectParams{Name: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.CreateApp(ctx, store.CreateAppParams{ProjectID: p, Name: "web", BuildStrategy: "dockerfile"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetAppSentryKey(ctx, store.SetAppSentryKeyParams{ID: id, SentryKey: "abc"}); err != nil {
		t.Fatal(err)
	}
	app, _ := s.GetAppByID(ctx, id)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/{app_id}/envelope/", panelIngest(s))

	for _, name := range []string{"python_transaction", "python_error_in_transaction", "python_failed_transaction"} {
		body, err := os.ReadFile("../internal/ingest/testdata/" + name + ".envelope")
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("POST", fmt.Sprintf("/api/%d/envelope/", app.ID), bytes.NewReader(body))
		req.Header.Set("X-Sentry-Auth", "Sentry sentry_key=abc, sentry_version=7")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}

	routes, err := ops.RoutesOf(s, app, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 2 {
		t.Errorf("routes %+v, want /orders/{order_id} and /users/{id}/profile", routes)
	}
	kept, err := s.Tel.ListTraces(ctx, teldb.ListTracesParams{AppID: id, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 || kept[0].HttpStatus != 500 {
		t.Fatalf("kept %+v, want the failed request only", kept)
	}
	events, err := s.Tel.ListTelemetryEvents(ctx, teldb.ListTelemetryEventsParams{AppID: id, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].TraceID != kept[0].TraceID {
		t.Errorf("the error isn't linked to its trace: %+v", events)
	}
	if _, err := ops.TraceOf(s, app, kept[0].ID); err != nil {
		t.Error(err)
	}
}
