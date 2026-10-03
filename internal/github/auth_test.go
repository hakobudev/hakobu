package github

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A token to clone with reaches one repository's contents, read-only, and
// nothing else of the installation's.
func TestRepoTokenIsScopedToTheRepo(t *testing.T) {
	var asked map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /repos/acme/shop/installation":
			fmt.Fprint(w, `{"id":7}`)
		case "POST /app/installations/7/access_tokens":
			_ = json.NewDecoder(r.Body).Decode(&asked)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"token":"ghs_scoped"}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	old := apiURL
	apiURL = srv.URL
	defer func() { apiURL = old }()

	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	token, err := RepoToken(1, string(keyPEM), "acme/shop")
	if err != nil || token != "ghs_scoped" {
		t.Fatalf("token %q, %v", token, err)
	}
	if got := fmt.Sprint(asked); got != "map[permissions:map[contents:read] repositories:[shop]]" {
		t.Errorf("asked for %s", got)
	}
}
