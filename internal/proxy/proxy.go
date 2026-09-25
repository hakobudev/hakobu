// Package proxy runs one in-process reverse proxy per app on its stable
// 127.0.0.1 port. Swapping the target is what makes blue/green deploys
// zero-downtime: in-flight requests finish on the old container.
package proxy

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
)

type appProxy struct {
	mu     sync.RWMutex
	target *url.URL
}

func (p *appProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.RLock()
	target := p.target
	p.mu.RUnlock()
	if target == nil {
		http.Error(w, "no backend available yet", http.StatusBadGateway)
		return
	}
	httputil.NewSingleHostReverseProxy(target).ServeHTTP(w, r)
}

type entry struct {
	proxy    *appProxy
	listener net.Listener
}

var (
	mu       sync.Mutex
	registry = map[string]*entry{}
)

// Ensure starts the app's listener if it isn't running yet; isNew tells the
// caller to seed a target.
func Ensure(name string, port int64) (isNew bool, err error) {
	mu.Lock()
	defer mu.Unlock()
	if _, ok := registry[name]; ok {
		return false, nil
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false, err
	}
	p := &appProxy{}
	registry[name] = &entry{proxy: p, listener: ln}
	go http.Serve(ln, p)
	return true, nil
}

func SetTarget(name string, target *url.URL) error {
	mu.Lock()
	e, ok := registry[name]
	mu.Unlock()
	if !ok {
		return fmt.Errorf("no proxy running for app %q", name)
	}
	e.proxy.mu.Lock()
	e.proxy.target = target
	e.proxy.mu.Unlock()
	return nil
}

func Remove(name string) {
	mu.Lock()
	e, ok := registry[name]
	delete(registry, name)
	mu.Unlock()
	if ok {
		e.listener.Close()
	}
}
