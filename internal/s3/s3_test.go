package s3

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	c := New(srv.URL, "auto", "", "key", "secret")
	for bucket, want := range map[string]bool{"mine": true, "backups": false} {
		if got, err := c.CanList(bucket); err != nil || got != want {
			t.Errorf("CanList(%s) = %v, %v; want %v", bucket, got, err, want)
		}
	}
	if _, err := c.CanList("broken"); err == nil {
		t.Error("a server error read as an answer")
	}
}

func TestDeleteLocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/young") {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>ObjectLockedByBucketPolicy</Code><Message>The object is locked by the bucket policy.</Message></Error>`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := New(srv.URL, "", "backups", "key", "secret")
	if err := c.Delete(context.Background(), "young"); !errors.Is(err, ErrLocked) {
		t.Errorf("locked: %v", err)
	}
	if err := c.Delete(context.Background(), "other"); err == nil || errors.Is(err, ErrLocked) {
		t.Errorf("a failure: %v", err)
	}
}
