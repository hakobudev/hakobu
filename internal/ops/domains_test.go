package ops

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/store"
)

// The domains of the panel and the apps are looked up at their registries
// (through IANA's list, longest suffix first), and the ones expiring
// within 30 days are kept, with what they'd take down.
func TestCheckDomains(t *testing.T) {
	soon := time.Now().Add(5*24*time.Hour + time.Hour)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		event := func(at time.Time) map[string]any {
			return map[string]any{"events": []map[string]any{{"eventAction": "registration", "eventDate": "2020-01-01T00:00:00Z"}, {"eventAction": "expiration", "eventDate": at.UTC().Format(time.RFC3339)}}}
		}
		var v any
		switch r.URL.Path {
		case "/dns.json":
			v = map[string]any{"services": [][][]string{{{"uk"}, {srv.URL + "/wrong/"}}, {{"co.uk"}, {srv.URL + "/uk/"}}, {{"dev"}, {srv.URL + "/dev/"}}}}
		case "/uk/domain/example.co.uk":
			v = event(soon)
		case "/dev/domain/other.dev":
			v = event(time.Now().Add(200 * 24 * time.Hour))
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(v)
	}))
	defer srv.Close()
	old := rdapBootstrapURL
	rdapBootstrapURL = srv.URL + "/dns.json"
	t.Cleanup(func() {
		rdapBootstrapURL = old
		domainState.Lock()
		domainState.checked, domainState.expiring, domainState.servers = time.Time{}, nil, nil
		domainState.Unlock()
	})

	t.Chdir(t.TempDir())
	if err := config.SetPublicHost("hakobu.example.co.uk"); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProject(ctx(), "p"); err != nil {
		t.Fatal(err)
	}
	p, _ := s.GetProject(ctx(), "p")
	for name, domain := range map[string]string{"shop": "shop.example.co.uk", "blog": "www.other.dev", "quiet": ""} {
		if err := s.CreateApp(ctx(), store.CreateAppParams{ProjectID: p.ID, Name: name, BuildStrategy: "dockerfile"}); err != nil {
			t.Fatal(err)
		}
		if err := s.SetAppDomain(ctx(), store.SetAppDomainParams{Name: name, Domain: domain}); err != nil {
			t.Fatal(err)
		}
	}

	CheckDomains(s)
	got := ExpiringDomains()
	if len(got) != 1 || got[0].Domain != "example.co.uk" || !got[0].Expires.Equal(soon.Truncate(time.Second)) ||
		len(got[0].Uses) != 2 || got[0].Uses[0] != "the panel" || got[0].Uses[1] != "app shop" {
		t.Fatalf("expiring %+v", got)
	}
	if w := got[0].When(); w != "expires in 5 days" {
		t.Errorf("when: %s", w)
	}
	if w := (DomainExpiry{Expires: time.Now().Add(-30 * time.Hour)}).When(); w != "expired 1 day ago" {
		t.Errorf("when, lapsed: %s", w)
	}
}
