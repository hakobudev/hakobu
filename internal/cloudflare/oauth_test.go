package cloudflare

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTokenRequests(t *testing.T) {
	challenged := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if challenged {
			w.Header().Set("cf-mitigated", "challenge")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, "<html>Just a moment...</html>")
			return
		}
		_ = r.ParseForm()
		switch r.Form.Get("grant_type") {
		case "refresh_token": // Cloudflare may keep the refresh token
			fmt.Fprint(w, `{"access_token":"at-2","expires_in":3600}`)
		default:
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"invalid_grant","error_description":"the code was used"}`)
		}
	}))
	defer srv.Close()
	old := TokenURL
	TokenURL = srv.URL
	defer func() { TokenURL = old }()

	if tok, err := Refresh("cid", "rt-1"); err != nil || tok.AccessToken != "at-2" || tok.RefreshToken != "rt-1" {
		t.Errorf("refresh: %+v, %v", tok, err)
	}
	if _, err := Exchange("cid", "https://p/cloudflare/callback", "used", "v"); err == nil || !strings.Contains(err.Error(), "the code was used") {
		t.Errorf("exchange of a used code: %v", err)
	}
	challenged = true
	if _, err := Refresh("cid", "rt-1"); err == nil || !strings.Contains(err.Error(), "challenges this server's network") {
		t.Errorf("challenged: %v", err)
	}
}
