package apply

import (
	"context"
	"io"
	"slices"
	"strings"
	"time"
)

// Step is one thing a deploy did, in order: plan, data, pull, dump,
// release, start, switch, stop, worker, remove.
type Step struct {
	Name   string        `json:"name"`
	Detail string        `json:"detail,omitempty"`
	Took   time.Duration `json:"took,omitempty"`
	Error  string        `json:"error,omitempty"`
}

// Log is a deploy's output: the text, and the steps it took.
type Log struct {
	strings.Builder
	Steps    []Step
	progress *Progress
}

// Progress is told of a deploy as it goes: as it starts, with what it
// deploys, and as each step starts and ends.
type Progress struct {
	Start func(Result)
	Step  func(s Step, done bool)
}

type progressKey struct{}

// WithProgress has the deploys and rollbacks run with ctx tell p.
func WithProgress(ctx context.Context, p *Progress) context.Context {
	return context.WithValue(ctx, progressKey{}, p)
}

// ProgressOf is what deploys run with ctx tell, if anything.
func ProgressOf(ctx context.Context) *Progress {
	p, _ := ctx.Value(progressKey{}).(*Progress)
	return p
}

func (l *Log) start(r Result) {
	if l.progress != nil && l.progress.Start != nil {
		l.progress.Start(r)
	}
}

func (l *Log) tell(s Step, done bool) {
	if l.progress != nil && l.progress.Step != nil {
		l.progress.Step(s, done)
	}
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
	l.tell(l.Steps[i], false)
	return func(err error) error {
		l.Steps[i].Took = time.Since(start)
		if err != nil {
			l.Steps[i].Error = err.Error()
		}
		l.tell(l.Steps[i], true)
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
