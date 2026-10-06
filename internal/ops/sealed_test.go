package ops

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/x0ryz/hakobu/internal/store"
)

func TestSealedVars(t *testing.T) {
	// DeleteApp below updates the watchdog after it returns: done before
	// the test ends, its writes don't race the cleanup of its directory.
	old := async
	async = func(f func()) { f() }
	t.Cleanup(func() { async = old })
	s, err := store.Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(must2(CreateProject(s, 0, "p")))
	p, _ := getProject(s, "p")
	must(createApp(s, store.CreateAppParams{ProjectID: p.ID, Name: "web", BuildStrategy: "dockerfile"}))

	must(SetSharedEnv(s, projectID(t, s, "p"), "SHARED=1\nOVERRIDDEN=project"))
	must(SealVar(s, "project", projectID(t, s, "p"), "PROJECT_SECRET", "ps"))
	must(SealVar(s, "project", projectID(t, s, "p"), "SHADOWED", "sealed"))
	must(SetAppEnv(s, appID(t, s, "web"), "VISIBLE=v\nSHADOWED=visible-app"))
	must(SealVar(s, "app", appID(t, s, "web"), "API_KEY", "sk-live"))
	must(SealVar(s, "app", appID(t, s, "web"), "OVERRIDDEN", "app-sealed"))

	// A name can't be visible and sealed in the same scope.
	if err := SealVar(s, "app", appID(t, s, "web"), "VISIBLE", "x"); err == nil {
		t.Error("sealed a name that's visible in the same scope")
	}
	if err := SetAppEnv(s, appID(t, s, "web"), "API_KEY=plain"); err == nil {
		t.Error("a visible variable took a sealed name")
	}
	if err := SealVar(s, "app", 999, "K", "v"); err == nil {
		t.Error("sealed a variable of a missing app")
	}
	if err := SealVar(s, "app", appID(t, s, "web"), "1BAD", "v"); err == nil {
		t.Error("accepted an invalid name")
	}

	app, _ := getApp(s, "web")
	env, err := appEnv(s, app, 8080)
	must(err)
	last := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		last[k] = v
	}
	for k, want := range map[string]string{"API_KEY": "sk-live", "PROJECT_SECRET": "ps", "SHADOWED": "visible-app", "OVERRIDDEN": "app-sealed", "SHARED": "1"} {
		if last[k] != want {
			t.Errorf("container gets %s=%q, want %q", k, last[k], want)
		}
	}

	// The panel's view: sealed values never leave the server.
	shown, err := EffectiveEnv(s, app)
	must(err)
	for _, e := range shown {
		switch e.Key {
		case "API_KEY", "PROJECT_SECRET", "OVERRIDDEN":
			if !e.Sealed || e.Value != "" {
				t.Errorf("%s shown as %+v", e.Key, e)
			}
		case "SHADOWED":
			if e.Sealed || e.Value != "visible-app" {
				t.Errorf("a visible app variable over a sealed project one: %+v", e)
			}
		}
	}
	if keys := SealedKeys(s, "app", appID(t, s, "web")); !slices.Equal(keys, []string{"API_KEY", "OVERRIDDEN"}) {
		t.Errorf("sealed keys = %v", keys)
	}

	// Deleting the app takes its sealed variables along.
	must(DeleteApp(s, appID(t, s, "web")))
	if vars, _ := s.ListAllSealedVars(ctx()); len(vars) != 2 {
		t.Errorf("%d sealed vars left, want the project's 2", len(vars))
	}
}

// ChangeAppVars edits the variables it names and keeps the rest of the
// text, comments included.
func TestChangeAppVars(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CreateProject(s, 0, "p"); err != nil {
		t.Fatal(err)
	}
	p, _ := getProject(s, "p")
	if err := createApp(s, store.CreateAppParams{ProjectID: p.ID, Name: "web", BuildStrategy: "dockerfile"}); err != nil {
		t.Fatal(err)
	}
	if err := SetAppEnv(s, appID(t, s, "web"), "# mine\nA=1\nB=2\nexport C=3"); err != nil {
		t.Fatal(err)
	}
	if err := SealVar(s, "app", appID(t, s, "web"), "KEY", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := ChangeAppVars(s, appID(t, s, "web"), map[string]string{"B": "two", "D": "4", "C": "three"}, []string{"A"}); err != nil {
		t.Fatal(err)
	}
	app, _ := getApp(s, "web")
	if got, want := string(app.Env), "# mine\nB=two\nC=three\nD=4"; got != want {
		t.Errorf("env = %q, want %q", got, want)
	}
	for _, bad := range []map[string]string{{"KEY": "plain"}, {"1X": "y"}, {"X": "a\nY=b"}} {
		if err := ChangeAppVars(s, appID(t, s, "web"), bad, nil); err == nil {
			t.Errorf("took %v", bad)
		}
	}
}
