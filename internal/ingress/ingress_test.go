package ingress

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"
)

// app is a backend on 127.0.0.1 that says who it is and what it was asked.
func app(t *testing.T, name string) int64 {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s %s %s %s", name, r.Host, r.URL.Path, r.Header.Get("X-Forwarded-Proto"))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	return int64(port)
}

func get(t *testing.T, h http.Handler, host, path string) (int, string) {
	req := httptest.NewRequest("GET", "http://"+host+path, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

func TestRouter(t *testing.T) {
	routes := map[string]int64{"crm.example.com": app(t, "crm"), "admin.example.com": app(t, "admin")}
	r := Router(func() map[string]int64 { return routes })
	if code, body := get(t, r, "crm.example.com", "/cars"); code != 200 || body != "crm crm.example.com /cars http" {
		t.Errorf("%d %q", code, body)
	}
	if _, body := get(t, r, "ADMIN.example.com:443", "/"); body[:5] != "admin" {
		t.Errorf("by case and port: %q", body)
	}
	if code, _ := get(t, r, "other.example.com", "/"); code != http.StatusNotFound {
		t.Errorf("unknown host: %d", code)
	}
	routes["down.example.com"] = 1 // nothing listens
	if code, _ := get(t, r, "down.example.com", "/"); code != http.StatusBadGateway {
		t.Errorf("app down: %d", code)
	}
}

func TestTunnel(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	routes := map[string]int64{"crm.example.com": app(t, "crm")}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, Config{Mode: Tunnel, Listen: addr}, func() map[string]int64 { return routes }, t.TempDir(), io.Discard)
	}()
	var resp *http.Response
	var err error
	for range 50 {
		req, _ := http.NewRequest("GET", "http://"+addr+"/x", nil)
		req.Host = "crm.example.com"
		if resp, err = http.DefaultClient.Do(req); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "crm crm.example.com /x http" {
		t.Errorf("%q", b)
	}
	cancel()
	if err := <-done; err != nil {
		t.Error(err)
	}
}

func TestConfig(t *testing.T) {
	for _, c := range []Config{{Mode: "cloud"}, {Mode: Tunnel, Listen: "0.0.0.0:8080"}, {Mode: Tunnel, Listen: "100.64.0.1:8080"}} {
		if c.Check() == nil {
			t.Errorf("%+v passed", c)
		}
	}
	for _, c := range []Config{{}, {Mode: Public}, {Mode: Tunnel}, {Mode: Tunnel, Listen: "127.0.0.1:9000"}} {
		if err := c.Check(); err != nil {
			t.Errorf("%+v: %v", c, err)
		}
	}
	if p := (Config{Mode: Public}).Ports(); len(p) != 2 {
		t.Errorf("public ports %v", p)
	}
}

// The visitor's address and scheme reach the app through cloudflared on
// loopback, but a client elsewhere can't put ones of its choosing.
func TestForwardedFor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, r.Header.Get("X-Forwarded-For"), " ", r.Header.Get("X-Forwarded-Proto"))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	r := Router(func() map[string]int64 { return map[string]int64{"crm.example.com": int64(port)} })
	for remote, want := range map[string]string{
		"127.0.0.1:41234":   "203.0.113.7, 127.0.0.1 https",
		"[::1]:41234":       "203.0.113.7, ::1 https",
		"198.51.100.9:5555": "198.51.100.9 http",
	} {
		req := httptest.NewRequest("GET", "http://crm.example.com/", nil)
		req.RemoteAddr = remote
		req.Header.Set("X-Forwarded-For", "203.0.113.7")
		req.Header.Set("X-Forwarded-Proto", "https")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if got := w.Body.String(); got != want {
			t.Errorf("from %s: %q, want %q", remote, got, want)
		}
	}
}
