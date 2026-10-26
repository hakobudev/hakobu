package apply

import (
	"context"
	"strings"
	"testing"

	"github.com/hakobudev/hakobu/internal/deploy"
)

func steps(l *Log) string {
	var b []string
	for _, s := range l.Steps {
		line := s.Name + " " + s.Detail
		if s.Error != "" {
			line += " ✕"
		}
		b = append(b, line)
	}
	return strings.Join(b, "\n")
}

func TestApplySteps(t *testing.T) {
	h := newHarness(t)
	toml := strings.Replace(crm, "[apps.crm]", "[apps.crm]\nrelease = \"migrate\"", 1)
	var l Log
	if _, err := h.a.Apply(context.Background(), Release{Spec: parse(t, toml), Images: map[string]string{"crm": "reg/crm@sha256:aaa"}}, &l); err != nil {
		t.Fatal(err)
	}
	want := `plan main, cache, crm/uploads, crm
data main, cache
pull crm · reg/crm
dump main, before the release command
release crm · migrate
start crm
switch crm
worker crm`
	if got := steps(&l); got != want {
		t.Errorf("steps:\n%s\nwant:\n%s", got, want)
	}
	if len(l.Steps) != 8 {
		t.Errorf("%d steps, each once", len(l.Steps))
	}
	if !strings.Contains(l.String(), "pulling reg/crm@sha256:aaa") {
		t.Errorf("the text log lost its lines:\n%s", l.String())
	}

	// A release that fails is the last step, marked, with its last words.
	h.failRun = deploy.ErrReleaseFailed
	l = Log{}
	if _, err := h.a.Apply(context.Background(), Release{Spec: parse(t, toml), Images: map[string]string{"crm": "reg/crm@sha256:bbb"}}, &l); err == nil {
		t.Fatal("no error")
	}
	last := l.Steps[len(l.Steps)-1]
	if last.Name != "release" || !strings.Contains(last.Error, "migrated") || strings.Contains(last.Error, "keeps running") {
		t.Errorf("last step %+v", last)
	}
}
