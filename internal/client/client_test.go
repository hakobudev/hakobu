package client

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Stream hands each line over as it comes, not once the answer ends.
func TestStream(t *testing.T) {
	seen := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/x-ndjson" {
			http.Error(w, `{"error":"no stream asked"}`, http.StatusBadRequest)
			return
		}
		fmt.Fprintln(w, `{"n":1}`)
		w.(http.Flusher).Flush()
		select {
		case <-seen:
		case <-time.After(5 * time.Second):
		}
		fmt.Fprintln(w, `{"n":2}`)
	}))
	defer srv.Close()
	c := &Client{Name: "nova-1", URL: srv.URL}
	var got []string
	start := time.Now()
	err := c.Stream(context.Background(), "/v1/deploy", map[string]string{}, "application/x-ndjson", func(b []byte) error {
		got = append(got, string(b))
		if len(got) == 1 {
			close(seen)
		}
		return nil
	})
	if err != nil || strings.Join(got, " ") != `{"n":1} {"n":2}` {
		t.Fatalf("%q, %v", got, err)
	}
	if time.Since(start) > 4*time.Second {
		t.Error("the first line came only with the last")
	}
	err = c.Stream(context.Background(), "/v1/deploy", nil, "text/plain", func([]byte) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "no stream asked") {
		t.Errorf("err = %v", err)
	}
}
