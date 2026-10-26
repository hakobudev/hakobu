// Package ingress takes the apps' traffic from the internet to their
// proxies, by the domain each app's hakobu.toml gives it. A public server
// answers on 80 and 443 itself, with Let's Encrypt certificates for those
// domains alone; one behind a Cloudflare Tunnel answers plain HTTP on
// loopback, where the server's cloudflared brings the requests.
package ingress

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/acme/autocert"
)

// Routes is each domain's proxy port on 127.0.0.1, as the apps are now.
type Routes func() map[string]int64

// Router sends a request to the app its Host names.
func Router(routes Routes) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.ToLower(r.Host)
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		port, ok := routes()[host]
		if !ok {
			http.Error(w, "no app here", http.StatusNotFound)
			return
		}
		target := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", port)}
		p := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(target)
				// Behind a tunnel the client is cloudflared on loopback, and
				// the visitor's address is in the X-Forwarded-For it sent:
				// keep it, or every visitor would be 127.0.0.1 to the app.
				// From anywhere else the header is the client's to forge.
				tunneled := fromLoopback(pr.In)
				if tunneled {
					pr.Out.Header["X-Forwarded-For"] = pr.In.Header["X-Forwarded-For"]
				}
				pr.SetXForwarded()
				// The visitor's https ends at Cloudflare: cloudflared says
				// which it was, or every app would think it served http
				// (cookies without Secure, links to http://).
				if p := pr.In.Header.Get("X-Forwarded-Proto"); tunneled && (p == "https" || p == "http") {
					pr.Out.Header.Set("X-Forwarded-Proto", p)
				}
				pr.Out.Host = pr.In.Host // the app sees its own domain
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				http.Error(w, "the app isn't answering", http.StatusBadGateway)
			},
		}
		p.ServeHTTP(w, r)
	})
}

func fromLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	return err == nil && ip != nil && ip.IsLoopback()
}

// Mode is how traffic reaches the server.
type Mode string

const (
	Public Mode = "public" // 80 and 443, certificates from Let's Encrypt
	Tunnel Mode = "tunnel" // a Cloudflare Tunnel to Listen on loopback
)

// Config is the server's [ingress].
type Config struct {
	Mode Mode `toml:"mode"`
	// Email is Let's Encrypt's contact for the certificates (public).
	Email string `toml:"email"`
	// Listen is where the tunnel's cloudflared sends requests (tunnel);
	// default 127.0.0.1:8080.
	Listen string `toml:"listen"`
}

func (c Config) Check() error {
	switch c.Mode {
	case "", Public, Tunnel:
	default:
		return fmt.Errorf("ingress.mode: %q is public or tunnel", c.Mode)
	}
	if c.Mode == Tunnel && c.Listen != "" {
		if h, _, err := net.SplitHostPort(c.Listen); err != nil || !net.ParseIP(h).IsLoopback() {
			return fmt.Errorf("ingress.listen: %s should be on loopback, where only the server's cloudflared reaches it", c.Listen)
		}
	}
	return nil
}

// Ports are the ones it opens to the internet, for the audit.
func (c Config) Ports() []int {
	if c.Mode == Public {
		return []int{80, 443}
	}
	return nil
}

// Serve takes the traffic until ctx ends; certDir keeps the certificates
// (public).
func Serve(ctx context.Context, c Config, routes Routes, certDir string, log io.Writer) error {
	router := Router(routes)
	var servers []*http.Server
	switch c.Mode {
	case "":
		return nil // the apps are reached on the server alone
	case Tunnel:
		listen := c.Listen
		if listen == "" {
			listen = "127.0.0.1:8080"
		}
		servers = append(servers, &http.Server{Addr: listen, Handler: router, ReadHeaderTimeout: 10 * time.Second})
	case Public:
		m := &autocert.Manager{
			Prompt: autocert.AcceptTOS,
			Email:  c.Email,
			Cache:  autocert.DirCache(certDir),
			// Certificates only for the apps' domains: anyone can point a
			// name at the server and ask.
			HostPolicy: func(_ context.Context, host string) error {
				if _, ok := routes()[strings.ToLower(host)]; !ok {
					return errors.New("not a domain of this server's apps")
				}
				return nil
			},
		}
		servers = append(servers,
			// 80 answers Let's Encrypt's challenges and sends the rest to
			// HTTPS.
			&http.Server{Addr: ":80", Handler: m.HTTPHandler(nil), ReadHeaderTimeout: 10 * time.Second},
			&http.Server{Addr: ":443", Handler: router, TLSConfig: &tls.Config{GetCertificate: m.GetCertificate, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1", "acme-tls/1"}}, ReadHeaderTimeout: 10 * time.Second},
		)
	}
	errs := make(chan error, len(servers))
	for _, s := range servers {
		go func() {
			var err error
			if s.TLSConfig != nil {
				err = s.ListenAndServeTLS("", "")
			} else {
				err = s.ListenAndServe()
			}
			if !errors.Is(err, http.ErrServerClosed) {
				errs <- fmt.Errorf("ingress on %s: %w", s.Addr, err)
			}
		}()
	}
	fmt.Fprintf(log, "ingress: %s\n", c.Mode)
	select {
	case <-ctx.Done():
	case err := <-errs:
		for _, s := range servers {
			_ = s.Close()
		}
		return err
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shutdown)
	}
	return nil
}
