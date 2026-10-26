package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hakobudev/hakobu/internal/apply"
)

func TestStream(t *testing.T) {
	d := &Daemon{Config: &Config{}}
	var progress *apply.Progress
	h := d.need(Operator)(func(w http.ResponseWriter, r *http.Request) (any, error) {
		return stream(w, r, func(ctx context.Context, send func(Event)) error {
			progress = apply.ProgressOf(ctx)
			progress.Start(apply.Result{Project: "crm"})
			progress.Step(apply.Step{Name: "pull"}, false)
			progress.Step(apply.Step{Name: "pull"}, true)
			send(Event{Result: map[string]any{"project": "crm"}})
			return errors.New("registry gone")
		})
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/deploy", nil)
	h.ServeHTTP(rec, req.WithContext(context.WithValue(req.Context(), peerKey{}, Operator)))
	if rec.Code != 200 || rec.Header().Get("Content-Type") != Streamed || !rec.Flushed {
		t.Fatalf("code %d, type %q, flushed %v", rec.Code, rec.Header().Get("Content-Type"), rec.Flushed)
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(rec.Body.String()), "\n") {
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		switch {
		case e.Start != nil:
			got = append(got, "start "+e.Start.Project)
		case e.Step != nil && e.Done:
			got = append(got, "done "+e.Step.Name)
		case e.Step != nil:
			got = append(got, "step "+e.Step.Name)
		case e.Result != nil:
			got = append(got, "result")
		default:
			got = append(got, "error "+e.Error)
		}
	}
	if want := "start crm|step pull|done pull|result|error registry gone"; strings.Join(got, "|") != want {
		t.Errorf("events %q", got)
	}
}
