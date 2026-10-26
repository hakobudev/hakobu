package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hakobudev/hakobu/internal/client"
	"github.com/hakobudev/hakobu/internal/server"
	"github.com/hakobudev/hakobu/internal/update"
)

// A server that's asked to update goes away while it restarts, then
// answers with how the update went.
func TestUpdateServerWaits(t *testing.T) {
	updatePoll = 10 * time.Millisecond
	t.Cleanup(func() { updatePoll = 3 * time.Second })
	for _, state := range []string{"updated", "rolled back"} {
		t.Run(state, func(t *testing.T) {
			before := time.Now().Add(-time.Hour)
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					_ = json.NewEncoder(w).Encode(map[string]string{"from": "v1.2.0", "to": "latest"})
					return
				}
				st := server.Status{Server: "s", Version: "v1.2.0", LastUpdate: &update.Status{At: before, State: "updated"}}
				switch calls.Add(1) {
				case 1: // before asking
				case 2:
					st.LastUpdate = &update.Status{At: time.Now(), State: "running"}
				case 3:
					http.Error(w, "restarting", http.StatusBadGateway)
					return
				default:
					st.LastUpdate = &update.Status{At: time.Now(), From: "v1.2.0", To: "v1.3.0", State: state, Message: "it didn't come up"}
				}
				_ = json.NewEncoder(w).Encode(st)
			}))
			defer srv.Close()
			c := &client.Client{Name: "s", URL: srv.URL, Token: "t"}
			st, err := updateServer(context.Background(), c, "", io.Discard)
			if st.To != "v1.3.0" || (err == nil) != (state == "updated") || err != nil && !strings.Contains(err.Error(), "didn't come up") {
				t.Errorf("%+v, %v", st, err)
			}
		})
	}
}
