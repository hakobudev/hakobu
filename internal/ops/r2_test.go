package ops

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/x0ryz/hakobu/internal/cloudflare"
	"github.com/x0ryz/hakobu/internal/store"
)

func TestR2Off(t *testing.T) {
	var on atomic.Bool
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if !on.Load() {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"success":false,"errors":[{"code":10042,"message":"Please enable R2 through the Cloudflare Dashboard."}]}`)
			return
		}
		fmt.Fprint(w, `{"success":true,"errors":[],"result":{}}`)
	}))
	t.Cleanup(srv.Close)
	old := cloudflare.APIURL
	cloudflare.APIURL = srv.URL
	t.Cleanup(func() { cloudflare.APIURL = old })
	c := cloudflare.Client{Token: "tok"}

	first, second := R2Off(c, "r2-acc"), R2Off(c, "r2-acc")
	if !first || !second || calls.Load() != 1 {
		t.Fatalf("off: %v %v in %d calls", first, second, calls.Load())
	}
	// Turned on: seen after the minute "off" holds, and for good.
	on.Store(true)
	r2State.Lock()
	r2State.offSeen["r2-acc"] = time.Now().Add(-r2OffRecheck)
	r2State.Unlock()
	first, second = R2Off(c, "r2-acc"), R2Off(c, "r2-acc")
	if first || second || calls.Load() != 2 {
		t.Errorf("on: off %v %v in %d calls", first, second, calls.Load())
	}

	// A storage in an account without R2 says where to turn it on.
	on.Store(false)
	s, err := store.Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCloudflareToken(ctx(), "tok"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCloudflareTunnel(ctx(), store.SaveCloudflareTunnelParams{AccountID: "r2-acc", TunnelID: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProject(ctx(), "shop"); err != nil {
		t.Fatal(err)
	}
	err = CreateStorage(s, "shop", store.Storage{Provider: "r2"})
	if err == nil || !strings.Contains(err.Error(), cloudflare.R2URL("r2-acc")) {
		t.Errorf("storage without R2: %v", err)
	}
	if !R2Off(c, "r2-acc") {
		t.Error("still taken as on after Cloudflare refused")
	}
}
