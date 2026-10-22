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
	routes := map[string]int64{"crm.kaslauto.com": app(t, "crm"), "admin.kaslauto.com": app(t, "admin")}
	r := Router(func() map[string]int64 { return routes })
	if code, body := get(t, r, "crm.kaslauto.com", "/cars"); code != 200 || body != "crm crm.kaslauto.com /cars http" {
		t.Errorf("%d %q", code, body)
	}
	if _, body := get(t, r, "ADMIN.kaslauto.com:443", "/"); body[:5] != "admin" {
		t.Errorf("by case and port: %q", body)
	}
	if code, _ := get(t, r, "other.example.com", "/"); code != http.StatusNotFound {
		t.Errorf("unknown host: %d", code)
	}
	routes["down.kaslauto.com"] = 1 // nothing listens
	if code, _ := get(t, r, "down.kaslauto.com", "/"); code != http.StatusBadGateway {
		t.Errorf("app down: %d", code)
	}
}

func TestTunnel(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	routes := map[string]int64{"crm.kaslauto.com": app(t, "crm")}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, Config{Mode: Tunnel, Listen: addr}, func() map[string]int64 { return routes }, t.TempDir(), io.Discard)
	}()
	var resp *http.Response
	var err error
	for range 50 {
		req, _ := http.NewRequest("GET", "http://"+addr+"/x", nil)
		req.Host = "crm.kaslauto.com"
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
	if string(b) != "crm crm.kaslauto.com /x http" {
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
