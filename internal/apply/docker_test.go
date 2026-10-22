package apply

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hakobudev/hakobu/internal/deploy"
	"github.com/hakobudev/hakobu/internal/node"
	"github.com/hakobudev/hakobu/internal/spec"
)

func TestMain(m *testing.M) {
	deploy.RunDialerIfChild() // rootless Docker: this binary is the dialer too
	os.Exit(m.Run())
}

// HAKOBU_DOCKER_TEST=1 go test ./internal/apply -run Docker -v
//
// A project with an app, a worker, a database, a cache and a volume, on the
// local Docker: deployed, deployed again on a new image, then a release
// command and a version that never gets healthy both fail without
// touching the live one.
func TestDockerApply(t *testing.T) {
	if os.Getenv("HAKOBU_DOCKER_TEST") == "" {
		t.Skip("set HAKOBU_DOCKER_TEST=1 to run against the local Docker")
	}
	b := make([]byte, 3)
	rand.Read(b)
	id := "zt" + hex.EncodeToString(b)
	ctx := context.Background()

	oldPG, oldWait, oldPort := node.PostgresContainer, node.CandidateWait, FirstProxyPort
	node.PostgresContainer, node.CandidateWait, FirstProxyPort = id+"-postgres", 10*time.Second, 18081
	a := &Applier{Node: node.Local{}, StateFile: filepath.Join(t.TempDir(), "state.json"), SnapshotDir: t.TempDir()}
	t.Cleanup(func() {
		for _, c := range []string{id + "-web-blue", id + "-web-green", id + "-web-worker", id + "-cache", node.PostgresContainer} {
			_ = deploy.RemoveContainer(ctx, c)
		}
		for _, v := range []string{node.PostgresContainer + "_data", id + "-cache_data", node.Volume(id+"-web", "data")} {
			_ = deploy.RemoveVolume(ctx, v)
		}
		for _, n := range []string{node.ProjectNetwork(id), node.ProjectEdge(id)} {
			_ = deploy.RemoveNetwork(ctx, n)
		}
		for _, v := range []string{"v1", "v2", "broken"} {
			_ = deploy.RemoveImage(ctx, "zt-apply:"+id+"-"+v)
		}
		for _, img := range []node.Image{node.Latest, node.Previous, node.Next} {
			_ = deploy.RemoveImage(ctx, node.Tag(id+"-web", img))
		}
		node.PostgresContainer, node.CandidateWait, FirstProxyPort = oldPG, oldWait, oldPort
	})

	v1, v2, broken := buildImage(t, id, "v1"), buildImage(t, id, "v2"), buildImage(t, id, "broken")
	// Images built here aren't in a registry: "pulling" one is tagging it.
	a.Pull = func(ctx context.Context, ref, tag string) error { return deploy.TagImage(ctx, ref, tag) }

	toml := func(release string) *spec.Project {
		p, err := spec.Parse([]byte(fmt.Sprintf(`
project = %q
server = "local"
[apps.web]
release = %q
worker = "trap 'exit 0' TERM; sleep 3600 & wait"
uses = ["main", "cache"]
volumes = { data = "/data" }
env = { GREETING = "hi" }
[databases.main]
[caches.cache]
memory = "32mb"
`, id, release)))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	var log strings.Builder
	apply := func(p *spec.Project, image string) error {
		log.Reset()
		_, err := a.Apply(ctx, Release{Spec: p, Images: map[string]string{"web": image}, Secrets: map[string]map[string]string{"web": {"TOKEN": "s3cret"}}}, &log)
		return err
	}

	// The release command sees the database and the variables.
	release := `test -n "$DATABASE_URL" && test "$REDIS_HOST" = ` + id + `-cache && test "$TOKEN" = s3cret && echo migrated`
	if err := apply(toml(release), v1); err != nil {
		t.Fatalf("first deploy: %v\n%s", err, &log)
	}
	if !strings.Contains(log.String(), "migrated") {
		t.Errorf("the release command didn't run:\n%s", &log)
	}
	if got := get(t, "/"); got != "v1" {
		t.Fatalf("serves %q", got)
	}
	if st, _ := deploy.ContainerStatus(ctx, id+"-web-worker"); st != "running" {
		t.Errorf("worker is %s", st)
	}

	if err := apply(toml(release), v2); err != nil {
		t.Fatalf("second deploy: %v\n%s", err, &log)
	}
	if got := get(t, "/"); got != "v2" {
		t.Errorf("serves %q after the second deploy", got)
	}

	// A failing release command stops the deploy before the new version.
	if err := apply(toml("echo no; exit 3"), v1); !errors.Is(err, deploy.ErrReleaseFailed) {
		t.Errorf("failing release: %v\n%s", err, &log)
	}
	if got := get(t, "/"); got != "v2" {
		t.Errorf("serves %q after a failed release", got)
	}

	// A version that never answers isn't switched to.
	if err := apply(toml(release), broken); err == nil {
		t.Errorf("a broken version went live:\n%s", &log)
	}
	if got := get(t, "/"); got != "v2" {
		t.Errorf("serves %q after a broken version", got)
	}

	s, _ := LoadState(a.StateFile)
	if w := s.Projects[id].Apps["web"]; w.Image != v2 || w.Previous != v1 {
		t.Errorf("state %+v", w)
	}
}

// buildImage builds an image that serves version at / once it starts, and
// returns its ID.
func buildImage(t *testing.T, id, version string) string {
	t.Helper()
	cmd := `mkdir -p /data && date >> /data/log; trap 'exit 0' TERM; httpd -f -p $PORT -h /www & wait`
	if version == "broken" {
		cmd = "trap 'exit 0' TERM; sleep 3600 & wait"
	}
	dir := t.TempDir()
	dockerfile := fmt.Sprintf("FROM busybox:1.36\nRUN mkdir -p /www && echo -n %s > /www/index.html && echo -n ok > /www/up\nENTRYPOINT [\"/bin/sh\", \"-c\"]\nCMD [%q]\n", version, cmd)
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	tag := "zt-apply:" + id + "-" + version
	if out, err := exec.Command("docker", "build", "-q", "-t", tag, dir).CombinedOutput(); err != nil {
		t.Fatalf("docker build: %v\n%s", err, out)
	}
	return deploy.ImageID(context.Background(), tag)
}

func get(t *testing.T, path string) string {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s", FirstProxyPort, path))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
