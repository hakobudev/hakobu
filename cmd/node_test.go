package cmd

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// The node passes envelopes on to the panel with what the panel checks
// (the DSN's key, the encoding), and turns them away while it has no link.
func TestForwardIngest(t *testing.T) {
	var seen string
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = r.Method + " " + r.URL.Path + " " + r.Header.Get("X-Sentry-Auth") + " " + r.Header.Get("Content-Encoding") + " " + string(b)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer panel.Close()
	var toPanel atomic.Pointer[http.Client]
	h := forwardIngest(&toPanel)

	send := func() int {
		req := httptest.NewRequest(http.MethodPost, "/api/3/envelope/?sentry_key=k", strings.NewReader("envelope"))
		req.Header.Set("X-Sentry-Auth", "Sentry sentry_key=k")
		req.Header.Set("Content-Encoding", "gzip")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := send(); code != http.StatusServiceUnavailable {
		t.Errorf("without a link: %d", code)
	}
	addr := strings.TrimPrefix(panel.URL, "http://")
	toPanel.Store(&http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}}})
	if code := send(); code != http.StatusAccepted {
		t.Errorf("with a link: %d", code)
	}
	if seen != "POST /api/3/envelope/ Sentry sentry_key=k gzip envelope" {
		t.Errorf("the panel got %q", seen)
	}
}
