package cmd

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The node passes envelopes on to the panel with what the panel checks
// (the DSN's key, the encoding); while the panel is away it keeps them,
// and sends them on once linked again, oldest first.
func TestForwardIngest(t *testing.T) {
	var seen []string
	answer := http.StatusAccepted
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if answer < 300 {
			seen = append(seen, r.Method+" "+r.URL.RequestURI()+" "+r.Header.Get("X-Sentry-Auth")+" "+r.Header.Get("Content-Encoding")+" "+string(b))
		}
		w.WriteHeader(answer)
	}))
	defer panel.Close()
	var toPanel atomic.Pointer[http.Client]
	spool := newIngestSpool(t.TempDir())
	h := forwardIngest(&toPanel, spool)

	send := func(body string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/3/envelope/?sentry_key=k", strings.NewReader(body))
		req.Header.Set("X-Sentry-Auth", "Sentry sentry_key=k")
		req.Header.Set("Content-Encoding", "gzip")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := send("first"); code != http.StatusOK || len(spool.files()) != 1 {
		t.Errorf("without a link: %d, kept %d", code, len(spool.files()))
	}
	addr := strings.TrimPrefix(panel.URL, "http://")
	linked := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}}}
	toPanel.Store(linked)
	answer = http.StatusServiceUnavailable // restarting
	if code := send("second"); code != http.StatusOK || len(spool.files()) != 2 {
		t.Errorf("with the panel down: %d, kept %d", code, len(spool.files()))
	}
	if n, err := spool.drain(linked); n != 0 || err == nil || len(spool.files()) != 2 {
		t.Errorf("drained %d to a panel that's down (%v), kept %d", n, err, len(spool.files()))
	}

	answer = http.StatusAccepted
	if code := send("third"); code != http.StatusAccepted {
		t.Errorf("with a link: %d", code)
	}
	if n, err := spool.drain(linked); n != 2 || err != nil || len(spool.files()) != 0 {
		t.Errorf("drained %d (%v), kept %d", n, err, len(spool.files()))
	}
	want := []string{"third", "first", "second"}
	for i, w := range want {
		if i >= len(seen) || seen[i] != "POST /api/3/envelope/?sentry_key=k Sentry sentry_key=k gzip "+w {
			t.Errorf("the panel got %q, want %v in order", seen, want)
			break
		}
	}
}

// The spool keeps the newest envelopes within its size and age, and drops
// one the panel refuses for good.
func TestIngestSpoolLimits(t *testing.T) {
	spool := newIngestSpool(t.TempDir())
	spool.maxBytes = 300
	for i := range 5 {
		if err := spool.put("/api/1/envelope/", http.Header{}, []byte(strings.Repeat(fmt.Sprint(i), 80))); err != nil {
			t.Fatal(err)
		}
	}
	files := spool.files()
	if len(files) != 2 {
		t.Fatalf("kept %d envelopes in 300 bytes", len(files))
	}
	if b, _ := os.ReadFile(filepath.Join(spool.dir, files[0].name)); !strings.HasSuffix(string(b), strings.Repeat("3", 80)) {
		t.Errorf("kept the oldest: %q", b)
	}
	old := time.Now().Add(-25 * time.Hour)
	_ = os.Chtimes(filepath.Join(spool.dir, files[0].name), old, old)
	refused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden) // the app is gone
	}))
	defer refused.Close()
	addr := strings.TrimPrefix(refused.URL, "http://")
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}}}
	if n, err := spool.drain(c); n != 0 || err != nil || len(spool.files()) != 0 {
		t.Errorf("drained %d (%v), kept %d", n, err, len(spool.files()))
	}
}
