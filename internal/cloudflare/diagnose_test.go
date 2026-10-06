package cloudflare

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRefusedTokenSaysWhy(t *testing.T) {
	var verify string // the account's tokens/verify answer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/accounts/acc/tokens/verify":
			fmt.Fprint(w, verify)
		case "/user/tokens/verify":
			fmt.Fprint(w, `{"success":false,"errors":[{"code":1000,"message":"Invalid API Token"}]}`)
		default:
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`)
		}
	}))
	defer srv.Close()
	old := APIURL
	APIURL = srv.URL
	defer func() { APIURL = old }()

	for _, c := range []struct{ name, verify, want string }{
		{"valid", `{"success":true,"result":{"id":"t","status":"active"}}`, TokenLacksPermission},
		{"disabled", `{"success":true,"result":{"id":"t","status":"disabled"}}`, "the token is disabled"},
		{"wrong address", `{"success":false,"errors":[{"code":9109,"message":"Cannot use the access token from location: 2a01:4f9::1"}]}`, "Client IP Address Filtering"},
		{"deleted", `{"success":false,"errors":[{"code":1000,"message":"Invalid API Token"}]}`, "doesn't exist any more"},
	} {
		verify = c.verify
		err := Client{Token: "tok", AccountID: "acc"}.call("GET", "/zones", nil, nil)
		if err == nil || !strings.Contains(err.Error(), "Authentication error") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want it to say %q", c.name, err, c.want)
		}
	}
}

// Without its account, an account's token can't be checked: the user's
// verify calls every such token invalid, which isn't that it's gone.
func TestAccountTokenWithoutAccountIsNotGone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/tokens/verify" {
			fmt.Fprint(w, `{"success":false,"errors":[{"code":1000,"message":"Invalid API Token"}]}`)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`)
	}))
	defer srv.Close()
	old := APIURL
	APIURL = srv.URL
	defer func() { APIURL = old }()

	err := Client{Token: "cfat_tok"}.call("POST", "/accounts/acc/cfd_tunnel", map[string]string{}, nil)
	if err == nil || strings.Contains(err.Error(), "doesn't exist any more") {
		t.Errorf("%v, want no diagnosis", err)
	}
	if err := (Client{Token: "tok"}).call("GET", "/zones", nil, nil); err == nil || !strings.Contains(err.Error(), "doesn't exist any more") {
		t.Errorf("a user's token: %v, want it gone", err)
	}
}

// R2's S3 keys of a token: its ID, from its account or else the user,
// and the SHA-256 of its value.
func TestR2Credentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/accounts/acc/tokens/verify":
			fmt.Fprint(w, `{"success":true,"result":{"id":"account-token-id","status":"active"}}`)
		case "/user/tokens/verify":
			fmt.Fprint(w, `{"success":true,"result":{"id":"user-token-id","status":"active"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	old := APIURL
	APIURL = srv.URL
	defer func() { APIURL = old }()

	id, secret, err := Client{Token: "tok", AccountID: "acc"}.R2Credentials()
	// sha256("tok")
	if err != nil || id != "account-token-id" || secret != "1a7674eb4ee78df7e1ac439a93c3fa8e3c945784d4dec9fd8e3011738b2f1d62" {
		t.Errorf("credentials %q %q, %v", id, secret, err)
	}
	if id, _, _ := (Client{Token: "tok"}).R2Credentials(); id != "user-token-id" {
		t.Errorf("a user's token: id %q", id)
	}
}
