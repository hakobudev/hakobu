package s3

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store"
)

func TestCanList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=key/") || r.URL.Query().Get("list-type") != "2" {
			t.Errorf("unsigned or not a listing: %s %s", r.Header.Get("Authorization"), r.URL)
		}
		switch r.URL.Path {
		case "/mine":
			w.WriteHeader(http.StatusOK)
		case "/backups":
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	c := NewClient(store.Storage{Provider: "s3", Endpoint: srv.URL, AccessKeyID: "key", SecretAccessKey: secret.String("secret")})
	for bucket, want := range map[string]bool{"mine": true, "backups": false} {
		if got, err := c.CanList(bucket); err != nil || got != want {
			t.Errorf("CanList(%s) = %v, %v; want %v", bucket, got, err, want)
		}
	}
	if _, err := c.CanList("broken"); err == nil {
		t.Error("a server error read as an answer")
	}
}

// The example of AWS's documentation for a presigned GET ("Authenticating
// Requests: Using Query Parameters").
func TestPresignMatchesAWSExample(t *testing.T) {
	got := Presign("https://examplebucket.s3.amazonaws.com", "us-east-1", "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		"GET", "/test.txt", 24*time.Hour, time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
	want := "https://examplebucket.s3.amazonaws.com/test.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20130524T000000Z&X-Amz-Expires=86400&X-Amz-SignedHeaders=host&X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	if got != want {
		t.Errorf("Presign =\n%s\nwant\n%s", got, want)
	}
}
