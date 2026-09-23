package cmd

import (
	"compress/gzip"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/x0ryz/hako/internal/ingest"
	"github.com/x0ryz/hako/internal/ops"
	"github.com/x0ryz/hako/internal/store"
)

// maxEnvelopeBytes caps decompressed envelope size — every official Sentry
// SDK gzips its envelope body by default, so without a decompressed-size
// guard a malicious or misbehaving client could send a small gzip bomb.
const maxEnvelopeBytes = 10 << 20 // 10 MiB, matching self-hosted Sentry's own limit

// readEnvelopeBody reads the request body, transparently gzip-decoding it
// when the client set Content-Encoding: gzip — which every official Sentry
// SDK does by default, so skipping this means every real SDK's envelopes
// fail to parse while a hand-crafted uncompressed test request works fine.
func readEnvelopeBody(r *http.Request) ([]byte, error) {
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

// sentryAuthKeyPattern pulls sentry_key out of the X-Sentry-Auth header
// every Sentry SDK sends by default, e.g.
// "Sentry sentry_version=7, sentry_key=abcd1234, sentry_client=..." — order
// and spacing of the comma-separated fields aren't guaranteed, so this
// matches the key regardless of position.
var sentryAuthKeyPattern = regexp.MustCompile(`sentry_key=([a-zA-Z0-9]+)`)

// sentryKeyFromRequest extracts the DSN public key an SDK sent, checking
// the same places real Sentry SDKs use, in the same priority order as
// self-hosted Sentry: the auth header first, then the legacy query param.
func sentryKeyFromRequest(r *http.Request) string {
	if m := sentryAuthKeyPattern.FindStringSubmatch(r.Header.Get("X-Sentry-Auth")); m != nil {
		return m[1]
	}
	return r.URL.Query().Get("sentry_key")
}

// ingestLimiter is a minimal per-IP token bucket: capacity burst tokens,
// refilled at rate tokens/sec. Prevents a single client from flooding the
// public DSN endpoint and growing SQLite unboundedly. Idle buckets are swept
// periodically (see startIngestLimiterSweeper) so the map can't grow without
// bound from a client that hammers the endpoint from many distinct source
// ports/connections and then goes away.
type ingestLimiter struct {
	mu      sync.Mutex
	buckets map[string]*ingestBucket
	rate    float64
	burst   int
}

type ingestBucket struct {
	tokens float64
	last   time.Time
}

var globalIngestLimiter = &ingestLimiter{
	buckets: map[string]*ingestBucket{},
	rate:    10, // 10 req/sec sustained
	burst:   30,
}

func init() {
	globalIngestLimiter.startSweeper()
}

// ingestBucketIdleTTL is how long a bucket can sit untouched before the
// sweeper reclaims it — long enough that a client sending bursts every few
// minutes still keeps its accumulated state, short enough that scanning many
// distinct IPs doesn't leave the map growing forever.
const ingestBucketIdleTTL = 10 * time.Minute

func (l *ingestLimiter) startSweeper() {
	go func() {
		ticker := time.NewTicker(ingestBucketIdleTTL)
		defer ticker.Stop()
		for range ticker.C {
			cutoff := time.Now().Add(-ingestBucketIdleTTL)
			l.mu.Lock()
			for ip, b := range l.buckets {
				if b.last.Before(cutoff) {
					delete(l.buckets, ip)
				}
			}
			l.mu.Unlock()
		}
	}()
}

func (l *ingestLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[ip]
	now := time.Now()
	if !ok {
		b = &ingestBucket{tokens: float64(l.burst), last: now}
		l.buckets[ip] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	b.last = now
	b.tokens += elapsed * l.rate
	if b.tokens > float64(l.burst) {
		b.tokens = float64(l.burst)
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// clientIP identifies the caller for rate-limiting purposes. It deliberately
// ignores X-Forwarded-For: this endpoint is public and unauthenticated, and
// hako has no notion of a trusted reverse proxy in front of it, so trusting
// a client-supplied header here would let a single caller mint unlimited
// distinct "IPs" (a new one per request) and both bypass the rate limit and
// grow the bucket map without bound. TCP's own RemoteAddr can't be spoofed
// the same way.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// registerIngestRoutes wires up the Sentry-envelope-compatible ingestion
// endpoint: every project gets its own auto-issued DSN (see
// ops.ProjectSentryDSN), auto-injected as SENTRY_DSN into its deploys, so
// any official Sentry SDK sends errors and structured logs here without
// needing a Sentry account. Unlike /api/* (registerAPIRoutes), this is not
// bearer-token authed — the DSN's public key is the identifier, exactly
// like real Sentry, since it's meant to be embedded in client-side bundles.
func registerIngestRoutes(mux *http.ServeMux, s *store.Store) {
	mux.HandleFunc("POST /api/{project_id}/envelope/", func(w http.ResponseWriter, r *http.Request) {
		if !globalIngestLimiter.allow(clientIP(r)) {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		project, err := s.GetProjectBySentryProjectID(r.PathValue("project_id"))
		if err != nil || project.SentryKey == "" || project.SentryKey != sentryKeyFromRequest(r) {
			http.Error(w, "invalid dsn", http.StatusUnauthorized)
			return
		}

		body, err := readEnvelopeBody(r)
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

		for _, item := range items {
			switch item.Type {
			case "event":
				summary := ingest.ExtractEventSummary(item)
				if err := s.CreateTelemetryEvent(store.TelemetryEvent{
					Project: project.Name,
					Kind:    "error",
					Level:   summary.Level,
					Message: summary.Message,
					Payload: string(item.Payload),
				}); err != nil {
					fmt.Println("warning: failed to store telemetry event:", err)
				}
			case "log":
				for _, entry := range ingest.ExtractLogEntries(item) {
					if err := s.CreateTelemetryEvent(store.TelemetryEvent{
						Project: project.Name,
						Kind:    "log",
						Level:   entry.Level,
						Message: entry.Message,
						Payload: string(item.Payload),
					}); err != nil {
						fmt.Println("warning: failed to store telemetry log:", err)
					}
				}
			}
			// Other item types (transaction, profile, attachment, session,
			// replay, ...) are accepted and silently dropped for now —
			// traces/profiling are a deliberate scope cut (see memory), and
			// the SDK doesn't need an error back to keep working.
		}

		eventID, err := ops.RandomHex(16)
		if err != nil {
			eventID = "unknown"
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"` + eventID + `"}`))
	})
}
