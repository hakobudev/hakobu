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
	old := APIURL
	APIURL = srv.URL
	defer func() { APIURL = old }()

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

// A user sees the repositories of the App's installation on their own
// account, not those of anyone else's.
func TestListReposOfAnAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /app/installations":
			fmt.Fprint(w, `[{"id":1,"account":{"id":42}},{"id":2,"account":{"id":7}}]`)
		case "POST /app/installations/1/access_tokens":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"token":"t1"}`)
		case "POST /app/installations/2/access_tokens":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"token":"t2"}`)
		case "GET /installation/repositories":
			if r.Header.Get("Authorization") == "Bearer t1" {
				fmt.Fprint(w, `{"repositories":[{"full_name":"me/private"}]}`)
			} else {
				fmt.Fprint(w, `{"repositories":[{"full_name":"friend/site"}]}`)
			}
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	old := APIURL
	APIURL = srv.URL
	defer func() { APIURL = old }()

	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	for account, want := range map[int64]string{42: "[me/private]", 7: "[friend/site]", 9: "[]"} {
		repos, err := ListRepos(1, keyPEM, account)
		if err != nil || fmt.Sprint(repos) != want {
			t.Errorf("account %d: %v, %v; want %s", account, repos, err, want)
		}
	}
}
