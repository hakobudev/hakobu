package apply

import (
	"io"
	"slices"
	"strings"
	"time"
)

// Step is one thing a deploy did, in order: plan, data, pull, dump,
// release, start, switch, worker, remove.
type Step struct {
	Name   string        `json:"name"`
	Detail string        `json:"detail,omitempty"`
	Took   time.Duration `json:"took,omitempty"`
	Error  string        `json:"error,omitempty"`
}

// Log is a deploy's output: the text, and the steps it took.
type Log struct {
	strings.Builder
	Steps []Step
}

// step starts a step, if out is a Log, and returns what ends it: it
// takes the step's error, notes it and the time, and returns it.
func step(out io.Writer, name, detail string) func(error) error {
	l, ok := out.(*Log)
	if !ok {
		return func(err error) error { return err }
	}
	l.Steps = append(l.Steps, Step{Name: name, Detail: detail})
	i, start := len(l.Steps)-1, time.Now()
	return func(err error) error {
		l.Steps[i].Took = time.Since(start)
		if err != nil {
			l.Steps[i].Error = err.Error()
		}
		return err
	}
}

// names is what the changes are to, each once: web, admin, main.
func (p Plan) names() string {
	var out []string
	for _, c := range p.Changes {
		if !slices.Contains(out, c.Name) {
			out = append(out, c.Name)
		}
	}
	return strings.Join(out, ", ")
}
