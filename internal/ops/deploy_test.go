package ops

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/x0ryz/hakobu/internal/store"
)

func TestJobReservation(t *testing.T) {
	if ok, err := reserve("web", false); !ok || err != nil {
		t.Fatalf("first reserve = %v, %v", ok, err)
	}
	if ok, err := reserve("web", false); ok || err == nil {
		t.Error("a second manual deploy should be refused while one runs")
	}
	if ok, err := reserve("web", true); ok || err != nil {
		t.Errorf("a push during a deploy should be queued, got %v, %v", ok, err)
	}
	if !release("web") {
		t.Error("release should report the queued push")
	}
	if IsDeploying("web") {
		t.Error("app still marked busy after release")
	}
	if ok, _ := reserve("web", false); !ok || release("web") {
		t.Error("the queue should be empty after it was handed out")
	}
}

func TestHumanBytes(t *testing.T) {
	for in, want := range map[uint64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KB", 64 << 30: "64.0 GB"} {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestAddVolumeValidation(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := AddVolume(s, "web", "data", "/app/data/"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, path string }{
		{"data", "/other"},     // duplicate name
		{"files", "/app/data"}, // duplicate path
		{"files", "relative"},  // not absolute
		{"files", "/"},         // the whole filesystem
		{"files", "/a:/b"},     // bind syntax injection
		{"Bad_Name", "/files"}, // invalid name
	} {
		if err := AddVolume(s, "web", c.name, c.path); err == nil {
			t.Errorf("AddVolume(%q, %q) should fail", c.name, c.path)
		}
	}
	binds, _ := appBinds(s, "web")
	if len(binds) != 1 || binds[0] != "hakobu-vol-web_data:/app/data" {
		t.Errorf("binds = %v", binds)
	}
}

func TestDatabaseNames(t *testing.T) {
	for name, ok := range map[string]bool{
		"kasl-db": true, "main": true, "app_data": true, "db2": true,
		"2db": false, "Main": false, `a"b`: false, "a b": false, "a;drop": false, "": false,
	} {
		if validDBName.MatchString(name) != ok {
			t.Errorf("validDBName(%q) = %v, want %v", name, !ok, ok)
		}
	}
}

// A long build's log keeps its start and its end, and says what it cut.
func TestDeployLogCutsTheMiddle(t *testing.T) {
	l := &deployLog{}
	l.add([]byte("start\n"))
	line := []byte(strings.Repeat("x", 1023) + "\n")
	for range 4 * (deployLogTail + deployLogHead) / len(line) {
		l.add(line)
	}
	l.add([]byte("the end\n"))
	out := l.text()
	if !strings.HasPrefix(out, "start\n") || !strings.HasSuffix(out, "the end\n") || !strings.Contains(out, "bytes of output cut") {
		t.Fatalf("log lost its start, end or cut marker: %q...%q", out[:20], out[len(out)-20:])
	}
	if len(out) > deployLogHead+deployLogTail+100 {
		t.Fatalf("log is %d bytes", len(out))
	}
	if len(l.tail) > 2*deployLogTail {
		t.Fatalf("kept %d bytes of tail", len(l.tail))
	}
}

// A build gets only Railpack's settings and the public variables frontend
// tools build in, the app's over its project's, references filled in.
func TestBuildEnv(t *testing.T) {
	s := notifyStore(t)
	if err := CreateProject(s, 0, "shop"); err != nil {
		t.Fatal(err)
	}
	p, _ := s.GetProject(ctx(), "shop")
	for _, name := range []string{"web", "api"} {
		if err := s.CreateApp(ctx(), store.CreateAppParams{ProjectID: p.ID, Name: name, BuildStrategy: "railpack"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetAppDomain(ctx(), store.SetAppDomainParams{Name: "api", Domain: "api.example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := SetSharedEnv(s, "shop", "RAILPACK_NODE_VERSION=20\nRAILPACK_START_CMD=node shared.js\nSHARED_SECRET=x"); err != nil {
		t.Fatal(err)
	}
	if err := SetAppEnv(s, "web", "RAILPACK_START_CMD=node app.js\nAPI_KEY=y\nVITE_API_URL=${{api.URL}}/v1"); err != nil {
		t.Fatal(err)
	}
	app, _ := s.GetApp(ctx(), "web")
	env, err := buildEnv(s, app)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	if len(got) != 3 || got["RAILPACK_NODE_VERSION"] != "20" || got["RAILPACK_START_CMD"] != "node app.js" || got["VITE_API_URL"] != "https://api.example.com/v1" {
		t.Errorf("build env %q", env)
	}
}
