package cloudflare

import (
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"testing"
)

func TestPKCE(t *testing.T) {
	verifier, challenge := PKCE()
	sum := sha256.Sum256([]byte(verifier))
	if len(verifier) < 43 || challenge != base64.RawURLEncoding.EncodeToString(sum[:]) {
		t.Fatalf("bad PKCE pair %q %q", verifier, challenge)
	}
	u, _ := url.Parse(AuthorizeURL("id", "https://relay/cf/callback", "st", challenge))
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("scope") != scopes || q.Get("redirect_uri") != "https://relay/cf/callback" {
		t.Errorf("authorize URL = %s", u)
	}
}

func TestZoneFor(t *testing.T) {
	zones := []Zone{{ID: "1", Name: "example.com"}, {ID: "2", Name: "shop.example.com"}, {ID: "3", Name: "other.dev"}}
	for host, want := range map[string]string{
		"example.com":          "1",
		"a.example.com":        "1",
		"a.shop.example.com":   "2",
		"x.other.dev":          "3",
		"notexample.com":       "",
		"example.com.evil.net": "",
	} {
		z, ok := ZoneFor(zones, host)
		if z.ID != want || ok != (want != "") {
			t.Errorf("ZoneFor(%q) = %q, %v; want %q", host, z.ID, ok, want)
		}
	}
}
