// Package edge routes requests arriving through the Cloudflare Tunnel by
// Host header: an app's domain goes to its proxy port, anything else to the
// panel. Cloudflare terminates HTTPS, so this is plain HTTP on loopback.
package edge

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/x0ryz/hakobu/internal/store"
)

func Router(s *store.Store, panel http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.ToLower(r.Host)
		if i := strings.LastIndexByte(host, ':'); i != -1 {
			host = host[:i]
		}
		app, err := s.GetAppByDomain(r.Context(), host)
		if err != nil {
			panel.ServeHTTP(w, r)
			return
		}
		u, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", app.Port))
		httputil.NewSingleHostReverseProxy(u).ServeHTTP(w, r)
	})
}
