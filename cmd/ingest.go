package cmd

import (
	"compress/gzip"
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/x0ryz/hakobu/internal/ingest"
	"github.com/x0ryz/hakobu/internal/ops"
	"github.com/x0ryz/hakobu/internal/panellog"
	"github.com/x0ryz/hakobu/internal/store"
)

// maxEnvelopeBytes caps the decompressed size, guarding against gzip bombs.
const maxEnvelopeBytes = 10 << 20

var sentryKeyPattern = regexp.MustCompile(`sentry_key=([a-zA-Z0-9]+)`)

func sentryKey(r *http.Request) string {
	if m := sentryKeyPattern.FindStringSubmatch(r.Header.Get("X-Sentry-Auth")); m != nil {
		return m[1]
	}
	return r.URL.Query().Get("sentry_key")
}

func readEnvelope(r *http.Request) ([]byte, error) {
	reader := io.Reader(r.Body)
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		reader = gz
	}
	return io.ReadAll(io.LimitReader(reader, maxEnvelopeBytes+1))
}

// limiter is a per-app token bucket for the public ingest endpoint. Apps on
// the same server share an outgoing IP, so limiting by IP would let one
// noisy app starve the others.
type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

const (
	ingestRate  = 10.0
	ingestBurst = 30.0
)

func newLimiter() *limiter {
	l := &limiter{buckets: map[string]*bucket{}}
	go func() {
		for range time.Tick(10 * time.Minute) {
			l.mu.Lock()
			for key, b := range l.buckets {
				if time.Since(b.last) > 10*time.Minute {
					delete(l.buckets, key)
				}
			}
			l.mu.Unlock()
		}
	}()
	return l
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: ingestBurst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(ingestBurst, b.tokens+now.Sub(b.last).Seconds()*ingestRate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// ingestApp is an app as the ingest endpoint knows it: its name and the
// key of its DSN.
type ingestApp struct{ Name, Key string }

// ingestHandler accepts Sentry envelopes at POST /api/{app_id}/envelope/,
// the DSN every app gets as SENTRY_DSN, for the app find knows by its ID,
// and has keep store them where the app's telemetry is kept. It's served
// on the panel, publicly and for the apps on its server (ingest_relay.go),
// and on every other server for its own apps, with the same rate limits.
func ingestHandler(find func(ctx context.Context, id int64) (ingestApp, bool), keep func(ctx context.Context, app string, body []byte) error) http.HandlerFunc {
	lim := newLimiter()
	return func(w http.ResponseWriter, r *http.Request) {
		appID, _ := strconv.ParseInt(r.PathValue("app_id"), 10, 64)
		app, ok := find(r.Context(), appID)
		if !ok || app.Key == "" || subtle.ConstantTimeCompare([]byte(app.Key), []byte(sentryKey(r))) != 1 {
			http.Error(w, "invalid dsn", http.StatusUnauthorized)
			return
		}
		body, err := readEnvelope(r)
		if err != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}
		if len(body) > maxEnvelopeBytes {
			http.Error(w, "envelope too large", http.StatusRequestEntityTooLarge)
			return
		}
		items, err := ingest.ParseEnvelope(body)
		if err != nil {
			http.Error(w, "invalid envelope", http.StatusBadRequest)
			return
		}
		// Traces come with every sampled request: they get a budget of
		// their own, so a busy app's traces don't crowd out its errors.
		if !lim.allow(limitKey(app.Name, items)) {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		if err := keep(r.Context(), app.Name, body); err != nil {
			panellog.Error("ingest: failed to keep an envelope of", app.Name+":", err)
			http.Error(w, "not kept", http.StatusServiceUnavailable)
			return
		}
		id, _ := ops.RandomHex(16)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"%s"}`, id)
	}
}

// panelIngest is ingestHandler on the panel: it knows every app, and keeps
// what they send where their telemetry is (ops.StoreEnvelope).
func panelIngest(s *store.Store) http.HandlerFunc {
	return ingestHandler(func(ctx context.Context, id int64) (ingestApp, bool) {
		app, err := s.GetAppByID(ctx, id)
		if err != nil {
			return ingestApp{}, false
		}
		return ingestApp{app.Name, string(app.SentryKey)}, true
	}, func(_ context.Context, app string, body []byte) error {
		return ops.StoreEnvelope(s, app, body)
	})
}

// limitKey is the rate limit an envelope counts against: the app's own,
// or its traces' when it holds nothing but transactions.
func limitKey(app string, items []ingest.Item) string {
	for _, item := range items {
		if item.Type != "transaction" {
			return app
		}
	}
	if len(items) == 0 {
		return app
	}
	return app + " traces"
}
