// Package proxy is the in-process reverse proxy that lets hako swap
// which container serves a project's traffic atomically — the seam that
// makes blue/green deploys actually zero-downtime, instead of requiring a
// stop-old/start-new gap on the project's real port.
package proxy

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"
)

// trafficWindow is how many one-second buckets of request counts each
// project keeps in memory for the live traffic chart — old buckets are
// just overwritten in place, so memory use is fixed regardless of RPS.
const trafficWindow = 60

// healthcheckHeader mirrors deploy.HealthcheckHeader (duplicated as a
// literal rather than imported, to keep this low-level package from
// depending on the Docker-specific deploy package for one constant).
// Requests carrying it are hako's own periodic health polling, not
// real visitors — they still get proxied normally, just not counted.
const healthcheckHeader = "X-Hako-Healthcheck"

type trafficBucket struct {
	sec int64
	ok  int
	err int
}

// projectProxy holds the current backend target behind a mutex; ServeHTTP
// reads it once per request, so in-flight requests keep talking to
// whatever target they started with and only new requests see a swap.
type projectProxy struct {
	mu      sync.RWMutex
	target  *url.URL
	traffic [trafficWindow]trafficBucket
}

// statusRecorder captures the response status the backend actually sent,
// so traffic can be bucketed by outcome without buffering the body.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (p *projectProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.RLock()
	target := p.target
	p.mu.RUnlock()

	if target == nil {
		http.Error(w, "no backend available yet", http.StatusBadGateway)
		return
	}
	isHealthcheck := r.Header.Get(healthcheckHeader) != ""
	r.Header.Del(healthcheckHeader)
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	httputil.NewSingleHostReverseProxy(target).ServeHTTP(rec, r)
	if !isHealthcheck {
		p.recordRequest(rec.status)
	}
}

func (p *projectProxy) setTarget(u *url.URL) {
	p.mu.Lock()
	p.target = u
	p.mu.Unlock()
}

func (p *projectProxy) recordRequest(status int) {
	now := time.Now().Unix()
	idx := int(((now % trafficWindow) + trafficWindow) % trafficWindow)

	p.mu.Lock()
	b := &p.traffic[idx]
	if b.sec != now {
		*b = trafficBucket{sec: now}
	}
	if status >= 400 {
		b.err++
	} else {
		b.ok++
	}
	p.mu.Unlock()
}

type entry struct {
	proxy    *projectProxy
	listener net.Listener
}

var (
	mu       sync.Mutex
	registry = map[string]*entry{}
)

// Ensure makes sure a listener is running on port for project name.
// Idempotent — safe to call on every poller tick and before every deploy.
// isNew reports whether this call actually created the listener, so the
// caller knows to seed an initial target.
func Ensure(name string, port int) (isNew bool, err error) {
	mu.Lock()
	defer mu.Unlock()

	if _, ok := registry[name]; ok {
		return false, nil
	}

	p := &projectProxy{}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false, err
	}

	registry[name] = &entry{proxy: p, listener: ln}
	go http.Serve(ln, p)
	return true, nil
}

// SetTarget atomically switches which backend a project's proxy sends new
// requests to.
func SetTarget(name string, target *url.URL) error {
	mu.Lock()
	e, ok := registry[name]
	mu.Unlock()
	if !ok {
		return fmt.Errorf("no proxy running for project %q", name)
	}
	e.proxy.setTarget(target)
	return nil
}

// TrafficPoint is one second of a project's request traffic history.
type TrafficPoint struct {
	OK  int
	Err int
}

// Traffic returns the last trafficWindow seconds of request counts for a
// project, oldest first. Seconds with no proxy activity (or no proxy at
// all yet) come back as zero points, so callers can render a fixed-width
// chart without special-casing gaps.
func Traffic(name string) [trafficWindow]TrafficPoint {
	var out [trafficWindow]TrafficPoint

	mu.Lock()
	e, ok := registry[name]
	mu.Unlock()
	if !ok {
		return out
	}

	now := time.Now().Unix()
	e.proxy.mu.RLock()
	defer e.proxy.mu.RUnlock()
	for i := 0; i < trafficWindow; i++ {
		sec := now - int64(trafficWindow-1-i)
		idx := int(((sec % trafficWindow) + trafficWindow) % trafficWindow)
		b := e.proxy.traffic[idx]
		if b.sec == sec {
			out[i] = TrafficPoint{OK: b.ok, Err: b.err}
		}
	}
	return out
}

// Remove closes a deleted project's listener and frees its port for reuse.
func Remove(name string) {
	mu.Lock()
	e, ok := registry[name]
	delete(registry, name)
	mu.Unlock()
	if ok {
		e.listener.Close()
	}
}
