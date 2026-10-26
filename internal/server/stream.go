package server

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/hakobudev/hakobu/internal/apply"
)

// Event is a line of a deploy's or rollback's stream: one starts, a step
// starts or ends (done), one ended (result), or it stopped (error).
type Event struct {
	Start  *apply.Result  `json:"start,omitempty"`
	Step   *apply.Step    `json:"step,omitempty"`
	Done   bool           `json:"done,omitempty"`
	Result map[string]any `json:"result,omitempty"`
	Error  string         `json:"error,omitempty"`
}

// Streamed asks for a deploy's or rollback's events as they come, a JSON
// line each, instead of its results at the end.
const Streamed = "application/x-ndjson"

// streamed is what a handler returns once it has answered itself.
type streamed struct{}

// stream answers r with run's events as they come; run's error is the
// last.
func stream(w http.ResponseWriter, r *http.Request, run func(ctx context.Context, send func(Event)) error) (any, error) {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", Streamed)
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	send := func(e Event) {
		_ = enc.Encode(e)
		_ = rc.Flush()
	}
	ctx := apply.WithProgress(r.Context(), &apply.Progress{
		Start: func(res apply.Result) { send(Event{Start: &res}) },
		Step:  func(s apply.Step, done bool) { send(Event{Step: &s, Done: done}) },
	})
	if err := run(ctx, send); err != nil {
		send(Event{Error: err.Error()})
	}
	return streamed{}, nil
}
