package ci

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hakobudev/hakobu/internal/apply"
	"github.com/hakobudev/hakobu/internal/status"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, text := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// fakeDocker records the commands and answers buildx with a digest made
// from the tag.
type fakeDocker struct {
	calls  []string
	labels map[string]map[string]string // tag → labels
}

func (f *fakeDocker) run(ctx context.Context, dir string, log io.Writer, name string, args ...string) error {
	f.calls = append(f.calls, filepath.Base(dir)+": "+name+" "+args[0])
	if name != "docker" {
		return nil
	}
	var tag, meta string
	labels := map[string]string{}
	for i := 0; i < len(args)-1; i++ {
		switch args[i] {
		case "-t":
			tag = args[i+1]
		case "--metadata-file":
			meta = args[i+1]
		case "--label":
			k, v, _ := strings.Cut(args[i+1], "=")
			labels[k] = v
		}
	}
	if f.labels == nil {
		f.labels = map[string]map[string]string{}
	}
	f.labels[tag] = labels
	b, _ := json.Marshal(map[string]string{"containerimage.digest": fmt.Sprintf("sha256:%064x", len(tag))})
	return os.WriteFile(meta, b, 0o644)
}

func TestRelease(t *testing.T) {
	dir := t.TempDir()
	spec := "project = \"crm\"\nserver = \"kasl-1\"\n\n[apps.web]\n\n[apps.worker]\npath = \"jobs\"\n"
	write(t, dir, map[string]string{
		"hakobu.toml":         spec,
		"hakobu.secrets.toml": "",
		"Dockerfile":          "FROM busybox\n",
		"jobs/go.mod":         "module jobs\n",
	})
	f := &fakeDocker{}
	b := Build{Dir: dir, Image: "ghcr.io/kasl/crm", Tag: "main", Source: "https://github.com/kasl/crm", Revision: sha, Run: f.run}
	ref, err := b.Release(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ref, "ghcr.io/kasl/crm@sha256:") {
		t.Errorf("release %s", ref)
	}
	want := []string{
		filepath.Base(dir) + ": docker buildx",
		"jobs: railpack prepare",
		"jobs: docker buildx",
	}
	if len(f.calls) != 4 || !slices.Equal(f.calls[:3], want) || !strings.HasSuffix(f.calls[3], ": docker buildx") {
		t.Fatalf("calls %q", f.calls)
	}
	rel := f.labels["ghcr.io/kasl/crm:main"]
	if rel[apply.LabelSpec] != spec || rel[apply.LabelRevision] != sha || rel[apply.LabelSource] != "https://github.com/kasl/crm" {
		t.Errorf("release labels %v", rel)
	}
	if _, ok := rel[apply.LabelSecrets]; ok {
		t.Error("an empty secrets file became a label")
	}
	var images map[string]string
	if err := json.Unmarshal([]byte(rel[apply.LabelImages]), &images); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(images["web"], "ghcr.io/kasl/crm-web@sha256:") || !strings.HasPrefix(images["worker"], "ghcr.io/kasl/crm-worker@sha256:") {
		t.Errorf("images %v", images)
	}
	if f.labels["ghcr.io/kasl/crm-web:"+sha][apply.LabelRevision] != sha {
		t.Error("an app image has no revision")
	}

	// The release reads back as one the server takes.
	if _, meta, err := apply.ReadRelease(rel, nil); err == nil || meta.Revision != sha || !strings.Contains(err.Error(), "token") {
		t.Errorf("ReadRelease: %v, %+v", err, meta)
	}
}

func TestReleaseRefusesBadSpec(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{"hakobu.toml": "project = \"crm\"\nserver = \"kasl-1\"\n[apps.web]\ndomian = \"x\"\n"})
	f := &fakeDocker{}
	_, err := Build{Dir: dir, Image: "i", Tag: "main", Revision: sha, Run: f.run}.Release(context.Background())
	if err == nil || len(f.calls) != 0 {
		t.Errorf("err %v, calls %q", err, f.calls)
	}
}

type statuses []struct {
	state status.State
	desc  string
}

func (s *statuses) Get(ctx context.Context, c status.Commit) (status.State, string, bool, error) {
	if len(*s) == 0 {
		return "", "", false, nil
	}
	next := (*s)[0]
	if len(*s) > 1 {
		*s = (*s)[1:]
	}
	return next.state, next.desc, true, nil
}

func TestWait(t *testing.T) {
	c := status.Commit{Source: "https://github.com/kasl/crm", Revision: sha}
	ok := &statuses{{status.Pending, "deploying"}, {status.Success, "live"}}
	if err := Wait(context.Background(), ok, c, time.Second, time.Millisecond, io.Discard); err != nil {
		t.Error(err)
	}
	failed := &statuses{{status.Pending, "deploying"}, {status.Failure, "release: exit 1"}}
	if err := Wait(context.Background(), failed, c, time.Second, time.Millisecond, io.Discard); err == nil || !strings.Contains(err.Error(), "release: exit 1") {
		t.Error(err)
	}
	if err := Wait(context.Background(), &statuses{}, c, 20*time.Millisecond, time.Millisecond, io.Discard); err == nil || !strings.Contains(err.Error(), "no hakobu/deploy status") {
		t.Error(err)
	}
}
