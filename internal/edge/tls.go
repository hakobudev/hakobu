// Package edge is the optional public TLS entrypoint: a domain-name-based
// router that terminates HTTPS for the panel and/or any project that has a
// custom domain set, obtaining certificates automatically via Let's
// Encrypt. It's a plain extension of the process hako already runs — no
// separate reverse-proxy daemon (Caddy, nginx, …) to install or manage.
//
// It's always started; if nobody ever sets a public host or a project
// domain, autocertHostPolicy rejects every hostname, so no certificate is
// ever requested and the listeners just sit idle. Operators who expose
// things via Tailscale/Cloudflare tunnels instead are unaffected.
package edge

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/acme/autocert"

	"github.com/x0ryz/hako/internal/config"
	"github.com/x0ryz/hako/internal/store"
)

// Start launches the :80 (redirect) and :443 (auto-HTTPS reverse proxy)
// listeners in the background. panelAddr is the address the dashboard is
// actually bound to (config.AgentAddr, or an --addr/HAKO_ADDR override).
// Bind failures are logged and swallowed rather than returned: this is an
// optional convenience, not a required service, so a port already taken by
// something else (or no permission to bind low ports) shouldn't stop the
// agent from starting.
func Start(s *store.Store, panelAddr string) {
	mgr := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		Cache:      autocert.DirCache(filepath.Join("data", "certs")),
		HostPolicy: hostPolicy(s),
	}

	go func() {
		srv := &http.Server{Addr: ":443", Handler: router(s, panelAddr), TLSConfig: mgr.TLSConfig()}
		if err := srv.ListenAndServeTLS("", ""); err != nil {
			fmt.Println("edge: :443 listener not started:", err)
		}
	}()

	go func() {
		redirect := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://"+r.Host+r.URL.RequestURI(), http.StatusMovedPermanently)
		})
		if err := http.ListenAndServe(":80", redirect); err != nil {
			fmt.Println("edge: :80 listener not started:", err)
		}
	}()
}

// hostPolicy only allows certificate issuance for hostnames hako actually
// knows about (the configured public host, or a project's own domain) —
// otherwise anyone pointing random DNS at this server's IP could burn
// through Let's Encrypt's per-domain rate limit.
func hostPolicy(s *store.Store) autocert.HostPolicy {
	return func(ctx context.Context, host string) error {
		host = strings.ToLower(host)
		if host != "" && host == strings.ToLower(config.PublicBaseURL()) {
			return nil
		}
		if _, err := s.GetProjectByDomain(host); err == nil {
			return nil
		}
		return fmt.Errorf("edge: %q is not a configured hako domain", host)
	}
}

// router dispatches each request by Host header: the panel's own domain
// goes to the local dashboard port, a project's domain goes to that
// project's stable internal-proxy port (the same one blue/green deploys
// swap behind), and anything else is rejected outright.
func router(s *store.Store, panelAddr string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.ToLower(r.Host)
		if colonIdx := strings.LastIndexByte(host, ':'); colonIdx != -1 {
			host = host[:colonIdx]
		}

		var target *url.URL
		var err error
		if host == strings.ToLower(config.PublicBaseURL()) {
			target, err = url.Parse("http://" + panelAddr)
		} else if project, lookupErr := s.GetProjectByDomain(host); lookupErr == nil {
			target, err = url.Parse(fmt.Sprintf("http://127.0.0.1:%d", project.Port))
		} else {
			http.Error(w, "unknown host", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "bad upstream", http.StatusBadGateway)
			return
		}

		httputil.NewSingleHostReverseProxy(target).ServeHTTP(w, r)
	})
}
