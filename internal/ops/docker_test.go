package ops

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hakobudev/hakobu/internal/config"
	"github.com/hakobudev/hakobu/internal/deploy"
	"github.com/hakobudev/hakobu/internal/node"
	"github.com/hakobudev/hakobu/internal/store"
)

// TestDockerDeploys runs real containers: HAKOBU_DOCKER_TEST=1 go test ./internal/ops -run Docker -v
func TestDockerDeploys(t *testing.T) {
	if os.Getenv("HAKOBU_DOCKER_TEST") == "" {
		t.Skip("set HAKOBU_DOCKER_TEST=1 to run against the local Docker")
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	app := newDockerTestApp(t, s)
	t.Cleanup(func() { _ = DeleteApp(s, app.ID) })

	reload := func() store.App {
		a, err := getApp(s, app.Name)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	deployVersion := func(v string, wantErr bool) string {
		t.Helper()
		buildTestImage(t, nextImageTag(app), v)
		var out strings.Builder
		err := rollOut(s, reload(), node.Next, &out)
		if wantErr {
			if err == nil {
				t.Fatalf("deploy %s should fail:\n%s", v, out.String())
			}
			return out.String() + err.Error()
		}
		if err != nil {
			t.Fatalf("deploy %s: %v\n%s", v, err, out.String())
		}
		if err := promote(s, reload()); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}

	if err := AddVolume(s, app.ID, "data", "/data"); err != nil {
		t.Fatal(err)
	}

	// First deploy, port detected automatically.
	deployVersion("v1", false)
	expectServing(t, app, "v1")
	if a := reload(); a.LivePort != 8080 {
		t.Errorf("detected port %d, want 8080", a.LivePort)
	}
	if ok, why, checked := checkHealth(s, reload()); !ok || !checked {
		t.Errorf("the live app fails its health check: %q (checked %v)", why, checked)
	}
	checkStats(t, s, reload())
	v1 := deploy.ImageID(ctx(), ImageTag(app))

	// Volumes: recreate mode, data survives.
	out := deployVersion("v2", false)
	if !strings.Contains(out, "stopping") {
		t.Errorf("app with volumes should stop the old version first:\n%s", out)
	}
	expectServing(t, app, "v2")
	if got := dataLines(t, reload()); got != 2 {
		t.Errorf("volume has %d start entries, want 2 (data lost between deploys?)", got)
	}
	if n := countContainers(t, app.Key()+"-"); n != 1 {
		t.Errorf("%d app containers after deploy, want 1", n)
	}
	if deploy.ImageID(ctx(), PreviousImageTag(app)) != v1 {
		t.Error(":previous should be the v1 image")
	}

	// The worker mounts the same volume.
	if err := SaveWorker(s, store.Worker{AppID: app.ID, Name: "w", Command: "sleep 3600"}); err != nil {
		t.Fatal(err)
	}
	if m := dockerOut(t, "inspect", "-f", "{{range .Mounts}}{{.Name}}{{end}}", app.Key()+"-worker"); m != node.Volume(app.Key(), "data") {
		t.Errorf("worker mounts %q", m)
	}
	time.Sleep(2 * time.Second) // a command the entrypoint swallowed has exited by now
	if st := dockerOut(t, "inspect", "-f", "{{.State.Running}} {{.RestartCount}}", app.Key()+"-worker"); st != "true 0" {
		t.Errorf("the worker's command doesn't run: running, restarts = %s", st)
	}

	// A third build drops v1: only :latest and :previous are kept.
	deployVersion("v3", false)
	if deploy.ImageID(ctx(), v1) != "" {
		t.Error("the image that fell out of the rotation should be deleted")
	}

	// A broken build is rejected and the old version is started again.
	out = deployVersion("broken", true)
	if !strings.Contains(out, "started the previous version again") {
		t.Errorf("expected the old version to be restarted:\n%s", out)
	}
	expectServing(t, app, "v3")

	// A crash on start fails the deploy at once, with the app's output.
	start := time.Now()
	out = deployVersion("crashing", true)
	if time.Since(start) > 30*time.Second {
		t.Errorf("a crashing app took %v to be rejected", time.Since(start))
	}
	expectServing(t, app, "v3")

	// Limits reach the container; going over the memory limit fails the
	// deploy with the reason.
	if err := SetLimits(s, app.ID, 32, 0.5); err != nil {
		t.Fatal(err)
	}
	out = deployVersion("hungry", true)
	if !strings.Contains(out, "more than its 32 MB memory limit") {
		t.Errorf("expected an out-of-memory reason:\n%s", out)
	}
	expectServing(t, app, "v3")
	deployVersion("v3b", false)
	if got := dockerOut(t, "inspect", "-f", "{{.HostConfig.Memory}} {{.HostConfig.NanoCpus}} {{.HostConfig.RestartPolicy.Name}}", reload().ContainerName()); got != "33554432 500000000 unless-stopped" {
		t.Errorf("container limits and restart policy = %q", got)
	}
	if err := SetLimits(s, app.ID, 0, 0); err != nil {
		t.Fatal(err)
	}

	// Shared volumes keep the zero-downtime switch.
	if err := SetShareVolumes(s, app.ID, true); err != nil {
		t.Fatal(err)
	}
	if out := deployVersion("v4", false); strings.Contains(out, "stopping") {
		t.Errorf("shared volumes should not stop the old version:\n%s", out)
	}
	expectServing(t, app, "v4")

	// A start command runs instead of the image's.
	if err := SetAppBuild(s, app.ID, "", "dockerfile", `echo -n custom > /www/index.html; trap 'exit 0' TERM; httpd -f -p 8080 -h /www & wait`); err != nil {
		t.Fatal(err)
	}
	deployVersion("v5", false)
	expectServing(t, app, "custom")
	if err := SetAppBuild(s, app.ID, "", "dockerfile", ""); err != nil {
		t.Fatal(err)
	}
	deployVersion("v4", false)
	expectServing(t, app, "v4")

	// Rollback goes to v5 (now with the image's own start), rolling back
	// again returns to v4.
	for _, want := range []string{"v5", "v4"} {
		if err := StartRollback(s, app.ID, false); err != nil {
			t.Fatal(err)
		}
		waitIdle(t, app.ID)
		expectServing(t, app, want)
	}

	// Deleting the app removes containers, images and volumes.
	if err := DeleteApp(s, app.ID); err != nil {
		t.Fatal(err)
	}
	if n := countContainers(t, app.Key()+"-"); n != 0 {
		t.Errorf("%d containers left after delete", n)
	}
	if ok, _ := deploy.ImageExists(ctx(), ImageTag(app)); ok {
		t.Error("image left after delete")
	}
	if vols, _ := deploy.VolumeNames(ctx(), node.Volume(app.Key(), "")); len(vols) != 0 {
		t.Errorf("volumes left after delete: %v", vols)
	}
}

// After an agent dies mid-deploy, startup removes the slot the database
// doesn't name and starts the live one if a recreate deploy stopped it.
func TestDockerReconcileSlots(t *testing.T) {
	if os.Getenv("HAKOBU_DOCKER_TEST") == "" {
		t.Skip("set HAKOBU_DOCKER_TEST=1 to run against the local Docker")
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	app := newDockerTestApp(t, s)
	t.Cleanup(func() { _ = DeleteApp(s, app.ID) })
	buildTestImage(t, nextImageTag(app), "v1")
	var out strings.Builder
	if err := rollOut(s, app, node.Next, &out); err != nil {
		t.Fatalf("deploy: %v\n%s", err, out.String())
	}
	app, _ = getApp(s, app.Name)
	live, other := app.ContainerName(), app.Key()+"-green"
	if live == other {
		other = app.Key() + "-blue"
	}
	dockerOut(t, "run", "-d", "--name", other, "busybox:1.36", "sleep", "3600")
	dockerOut(t, "stop", "-t", "0", live)

	ReconcileSlots(s)
	if n := countContainers(t, other); n != 0 {
		t.Errorf("leftover slot %s still there", other)
	}
	if st, _ := deploy.ContainerStatus(ctx(), live); st != "running" {
		t.Errorf("live slot is %s, want running", st)
	}
}

// A stopped app stays stopped: through the reconcile at a restart and a
// push, until it's started.
func TestDockerStopApp(t *testing.T) {
	if os.Getenv("HAKOBU_DOCKER_TEST") == "" {
		t.Skip("set HAKOBU_DOCKER_TEST=1 to run against the local Docker")
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	app := newDockerTestApp(t, s)
	t.Cleanup(func() { _ = DeleteApp(s, app.ID) })
	buildTestImage(t, nextImageTag(app), "v1")
	var out strings.Builder
	if err := rollOut(s, app, node.Next, &out); err != nil {
		t.Fatalf("deploy: %v\n%s", err, out.String())
	}
	app, _ = getApp(s, app.Name)
	live := app.ContainerName()

	if err := StopApp(s, app.ID); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, app.ID)
	if st, _ := deploy.ContainerStatus(ctx(), live); st == "running" {
		t.Fatal("still running after Stop")
	}
	ReconcileSlots(s)
	if st, _ := deploy.ContainerStatus(ctx(), live); st == "running" {
		t.Error("the reconcile started a stopped app")
	}
	if err := StartDeploy(s, app.ID, "push"); err != nil || IsDeploying(app.ID) {
		t.Errorf("a push deployed a stopped app: %v", err)
	}
	if err := StopApp(s, app.ID); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, app.ID)
	if logs, _ := s.ListDeployLogs(ctx(), store.ListDeployLogsParams{AppID: app.ID, Limit: 1}); len(logs) == 0 || logs[0].Status != "failed" {
		t.Errorf("stopping a stopped app: %+v", logs)
	}

	if err := StartApp(s, app.ID); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, app.ID)
	if st, _ := deploy.ContainerStatus(ctx(), live); st != "running" {
		t.Errorf("after Start the app is %s", st)
	}
	if app, _ := getApp(s, app.Name); app.Stopped != 0 {
		t.Error("still marked stopped")
	}
}

// newDockerTestApp creates an app whose proxy port is free on this machine
// (a real hakobu may be using the first ones).
func newDockerTestApp(t *testing.T, s *store.Store) store.App {
	suffix, _ := RandomHex(3)
	project := "zt" + suffix
	if err := createProject(s, project); err != nil {
		t.Fatal(err)
	}
	p, _ := getProject(s, project)
	// Runs after the tests' own cleanups (LIFO), once the apps are gone.
	t.Cleanup(func() {
		if err := removeProjectNetworks(s, p); err != nil {
			t.Error(err)
		}
	})
	for i := 0; ; i++ {
		name := fmt.Sprintf("zt%s-%d", suffix, i)
		if err := createApp(s, store.CreateAppParams{ProjectID: p.ID, Name: name, BuildStrategy: "dockerfile"}); err != nil {
			t.Fatal(err)
		}
		app, _ := getApp(s, name)
		if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", app.Port)); err == nil {
			ln.Close()
			return app
		}
	}
}

func buildTestImage(t *testing.T, tag, version string) {
	t.Helper()
	// The shell stays PID 1 and exits on SIGTERM: a PID 1 without a handler
	// ignores it, and every stop would wait out Docker's 10 seconds.
	cmd := `mkdir -p /data && date >> /data/log; trap 'exit 0' TERM; httpd -f -p 8080 -h /www & wait`
	switch version {
	case "broken":
		cmd = "trap 'exit 0' TERM; sleep 3600 & wait" // never listens
	case "crashing":
		cmd = "echo missing POSTGRES_PASSWORD >&2; exit 1"
	case "hungry":
		cmd = "exec dd if=/dev/zero of=/dev/null bs=256M count=1"
	}
	dir := t.TempDir()
	// Started the way Railpack's images are: a shell entrypoint that
	// takes the command as Cmd.
	dockerfile := fmt.Sprintf("FROM busybox:1.36\nRUN mkdir /www && echo -n %s > /www/index.html\nENTRYPOINT [\"/bin/sh\", \"-c\"]\nCMD [%q]\n", version, cmd)
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	dockerOut(t, "build", "-q", "-t", tag, dir)
}

func expectServing(t *testing.T, app store.App, want string) {
	t.Helper()
	// What cloudflared sees: the app's alias on the edge network, served
	// by the live container only.
	edge := dockerOut(t, "run", "--rm", "--quiet", "--network", projectEdge(app.ProjectKey()), "busybox:1.36",
		"sh", "-c", fmt.Sprintf("nslookup %s 127.0.0.11 | grep -c '^Address' ; wget -qO- http://%s:8080/", EdgeAlias(app.Key()), EdgeAlias(app.Key())))
	if edge != "2\n"+want { // the resolver's own address, then exactly one container
		t.Errorf("edge network serves %q, want one address and %q", edge, want)
	}
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", app.Port))
	if err != nil {
		t.Fatalf("app proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != want {
		t.Errorf("serving %q, want %q", body, want)
	}
}

func dataLines(t *testing.T, app store.App) int {
	return len(strings.Split(strings.TrimSpace(dockerOut(t, "exec", app.ContainerName(), "cat", "/data/log")), "\n"))
}

func countContainers(t *testing.T, prefix string) int {
	n := 0
	for _, name := range strings.Fields(dockerOut(t, "ps", "-a", "--format", "{{.Names}}")) {
		if strings.HasPrefix(name, prefix) {
			n++
		}
	}
	return n
}

func waitIdle(t *testing.T, app int64) {
	for deadline := time.Now().Add(2 * time.Minute); IsDeploying(app); time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("job still running")
		}
	}
}

func dockerOut(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestDockerDataRollback runs its own Postgres container.
func TestDockerDataRollback(t *testing.T) {
	if os.Getenv("HAKOBU_DOCKER_TEST") == "" {
		t.Skip("set HAKOBU_DOCKER_TEST=1 to run against the local Docker")
	}
	suffix, _ := RandomHex(3)
	old := node.PostgresContainer
	node.PostgresContainer = "zt-pg-" + suffix
	t.Cleanup(func() {
		_ = deploy.RemoveContainer(ctx(), node.PostgresContainer)
		_ = deploy.RemoveVolume(ctx(), node.PostgresContainer+"_data")
		node.PostgresContainer = old
	})
	dir := t.TempDir()
	t.Chdir(dir) // snapshots go to data/snapshots

	s, err := store.Open(filepath.Join(dir, "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	app := newDockerTestApp(t, s)
	t.Cleanup(func() {
		if err := DeleteApp(s, app.ID); err != nil {
			t.Error(err)
		}
	})
	if _, err := CreateDatabase(s, app.ProjectID, "zt"+suffix); err != nil {
		t.Fatal(err)
	}
	if err := LinkDatabase(s, app, "zt"+suffix); err != nil {
		t.Fatal(err)
	}
	d, _ := getDB(s, "zt"+suffix)
	sql := func(q string) string {
		t.Helper()
		return dockerOut(t, "exec", node.PostgresContainer, "psql", "-U", d.Key(), "-d", d.Key(), "-Atc", q)
	}
	reload := func() store.App { a, _ := getApp(s, app.Name); return a }
	// StartDeploy's steps after the build.
	deployVersion := func(v string) {
		t.Helper()
		a := reload()
		buildTestImage(t, nextImageTag(a), v)
		var out strings.Builder
		snapshot := takeSnapshot(s, a, &out)
		if err := rollOut(s, a, node.Next, &out); err != nil {
			t.Fatalf("%v\n%s", err, out.String())
		}
		if err := promote(s, a); err != nil {
			t.Fatal(err)
		}
		if err := keepSnapshot(s, a, a.LinkedDbID, snapshot); err != nil {
			t.Fatal(err)
		}
	}
	rollBack := func(withData bool) {
		t.Helper()
		if err := StartRollback(s, app.ID, withData); err != nil {
			t.Fatal(err)
		}
		waitIdle(t, app.ID)
		logs, _ := s.ListDeployLogs(ctx(), store.ListDeployLogsParams{AppID: app.ID, Limit: 1})
		if logs[0].Status != "success" {
			t.Fatalf("rollback failed:\n%s", logs[0].Output)
		}
	}

	sql(`CREATE TABLE notes (s text); INSERT INTO notes VALUES ('v1 data')`)
	deployVersion("v1")
	// v2's "migration" breaks the data.
	deployVersion("v2")
	sql(`DROP TABLE notes; CREATE TABLE notes2 (s text); INSERT INTO notes2 VALUES ('v2 data')`)
	if reason := DataRollbackBlocker(s, reload()); reason != "" {
		t.Fatalf("data rollback blocked: %s", reason)
	}

	// Code and data go back to before v2; the v2 data becomes the snapshot.
	rollBack(true)
	expectServing(t, reload(), "v1")
	if got := sql(`SELECT string_agg(tablename, ',') FROM pg_tables WHERE schemaname = 'public'`); got != "notes" {
		t.Errorf("tables after data rollback: %q", got)
	}
	// And forward again.
	rollBack(true)
	expectServing(t, reload(), "v2")
	if got := sql(`SELECT s FROM notes2`); got != "v2 data" {
		t.Errorf("data after rolling forward: %q", got)
	}
	if got := sql(`SELECT count(*) FROM pg_database WHERE datname LIKE 'hakobu.%'`); got != "0" {
		t.Errorf("%s scratch databases left", got)
	}

	// A code-only rollback keeps the data and drops the snapshot, which no
	// longer matches :previous.
	rollBack(false)
	expectServing(t, reload(), "v1")
	if got := sql(`SELECT s FROM notes2`); got != "v2 data" {
		t.Errorf("code-only rollback changed the data: %q", got)
	}
	if DataRollbackBlocker(s, reload()) == "" {
		t.Error("data rollback offered without a snapshot")
	}

	// If the previous version doesn't start, the current data and version
	// come back.
	deployVersion("v3")
	sql(`INSERT INTO notes2 VALUES ('v3 data')`)
	a := reload()
	if err := s.SetAppSettings(ctx(), store.SetAppSettingsParams{ID: a.ID, HealthCheckPath: "/missing"}); err != nil { // 404 for every version
		t.Fatal(err)
	}
	if err := StartRollback(s, a.ID, true); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, a.ID)
	if logs, _ := s.ListDeployLogs(ctx(), store.ListDeployLogsParams{AppID: a.ID, Limit: 1}); logs[0].Status != "failed" || !strings.Contains(string(logs[0].Output), "put the current data back") {
		t.Errorf("failed rollback:\n%s", logs[0].Output)
	}
	if err := s.SetAppSettings(ctx(), store.SetAppSettingsParams{ID: a.ID, HealthCheckPath: "/"}); err != nil {
		t.Fatal(err)
	}
	expectServing(t, reload(), "v3")
	if got := sql(`SELECT count(*) FROM notes2`); got != "2" {
		t.Errorf("rows after a failed data rollback: %s, want 2", got)
	}

	// A database another app uses too is never rolled back.
	if err := createApp(s, store.CreateAppParams{ProjectID: app.ProjectID, Name: app.Name + "-b", BuildStrategy: "dockerfile"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := DeleteApp(s, appID(t, s, app.Name+"-b")); err != nil {
			t.Error(err)
		}
	})
	if err := LinkDatabase(s, mustApp(t, s, app.Name+"-b"), d.Name); err != nil {
		t.Fatal(err)
	}
	if reason := DataRollbackBlocker(s, reload()); !strings.Contains(reason, "other apps") {
		t.Errorf("shared database: %q", reason)
	}
}

// TestDockerRotateSecrets runs its own Postgres.
func TestDockerRotateSecrets(t *testing.T) {
	if os.Getenv("HAKOBU_DOCKER_TEST") == "" {
		t.Skip("set HAKOBU_DOCKER_TEST=1 to run against the local Docker")
	}
	suffix, _ := RandomHex(3)
	oldPG := node.PostgresContainer
	node.PostgresContainer = "zt-pg-" + suffix
	t.Cleanup(func() {
		_ = deploy.RemoveContainer(ctx(), node.PostgresContainer)
		_ = deploy.RemoveVolume(ctx(), node.PostgresContainer+"_data")
		node.PostgresContainer = oldPG
	})
	dir := t.TempDir()
	t.Chdir(dir)
	s, err := store.Open(filepath.Join(dir, "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	app := newDockerTestApp(t, s)
	t.Cleanup(func() {
		if err := DeleteApp(s, app.ID); err != nil {
			t.Error(err)
		}
	})
	must(must2(CreateDatabase(s, app.ProjectID, "zt"+suffix)))
	must(LinkDatabase(s, app, "zt"+suffix))
	p, _ := getProject(s, app.ProjectName)
	must(must2(s.CreateStorage(ctx(), store.CreateStorageParams{Name: "zt" + suffix, ProjectID: p.ID, Provider: "s3", Endpoint: "https://s3.example.com",
		AccessKeyID: "key", SecretAccessKey: "secret", Bucket: "files", Region: "auto"})))
	must(LinkStorage(s, app, "zt"+suffix))
	must(SetAppEnv(s, app.ID, "API_KEY=abc"))
	must(SealVar(s, "app", app.ID, "SEALED", "shh"))
	must(s.SetAppSentryKey(ctx(), store.SetAppSentryKeyParams{ID: app.ID, SentryKey: "oldsentrykey"}))
	must(s.NewSession(ctx(), "tok", 42, time.Hour))

	a, _ := getApp(s, app.Name)
	buildTestImage(t, nextImageTag(a), "v1")
	var out strings.Builder
	must(rollOut(s, a, node.Next, &out))
	must(promote(s, a))

	dbBefore, _ := getDB(s, "zt"+suffix)
	keyBefore, _ := os.ReadFile("master.key")

	var log strings.Builder
	manual, failures := rotateSecrets(s, &log)
	if failures != 0 {
		t.Fatalf("%d failures:\n%s", failures, log.String())
	}

	dbAfter, _ := getDB(s, "zt"+suffix)
	a, _ = getApp(s, app.Name)
	if dbAfter.Password == dbBefore.Password || a.SentryKey == "oldsentrykey" {
		t.Error("a secret wasn't replaced")
	}
	// The new database password works, the old one doesn't, connecting
	// like an app does (inside its container Postgres trusts localhost).
	login := func(pw string) error {
		return exec.Command("docker", "run", "--rm", "--quiet", "--network", ProjectNetwork(app.ProjectKey()), "-e", "PGPASSWORD="+pw, "postgres:18",
			"psql", "-h", node.PostgresContainer, "-U", dbAfter.Key(), "-d", dbAfter.Key(), "-c", "SELECT 1").Run()
	}
	if login(string(dbAfter.Password)) != nil || login(string(dbBefore.Password)) == nil {
		t.Error("database password not rotated")
	}
	// A rotation cut short after the panel stored the password: the next
	// start gives it to Postgres.
	must(local.SetPassword(ctx(), node.DBSpec{Name: dbAfter.Name, User: dbAfter.Key(), Password: "0123456789abcdef"}))
	SyncDatabasePasswords(s)
	if login(string(dbAfter.Password)) != nil {
		t.Error("startup didn't give Postgres the stored password")
	}
	// The app was restarted with the new values and still serves.
	expectServing(t, a, "v1")
	env, _ := deploy.ContainerEnv(ctx(), a.ContainerName())
	if !strings.Contains(env["DATABASE_URL"], string(dbAfter.Password)) || env["S3_ACCESS_KEY_ID"] != "key" || env["SEALED"] != "shh" {
		t.Errorf("the app runs with old values: DATABASE_URL=%q S3_ACCESS_KEY_ID=%q", env["DATABASE_URL"], env["S3_ACCESS_KEY_ID"])
	}
	if _, _, err := s.SessionUser(ctx(), "tok"); err == nil {
		t.Error("sessions survived")
	}
	if keyAfter, _ := os.ReadFile("master.key"); string(keyAfter) == string(keyBefore) {
		t.Error("master key not rotated")
	}
	if !slices.ContainsFunc(manual, func(m string) bool { return strings.Contains(m, "API_KEY") && strings.Contains(m, "SEALED") }) {
		t.Errorf("the user's variables aren't listed to replace: %v", manual)
	}
	if !slices.ContainsFunc(manual, func(m string) bool { return strings.Contains(m, "storage zt"+suffix) }) {
		t.Errorf("the storage's keys aren't listed to replace: %v", manual)
	}
}

func TestDockerProjectIsolation(t *testing.T) {
	if os.Getenv("HAKOBU_DOCKER_TEST") == "" {
		t.Skip("set HAKOBU_DOCKER_TEST=1 to run against the local Docker")
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	// newDockerTestApp puts each app in a project of its own.
	a, b := newDockerTestApp(t, s), newDockerTestApp(t, s)
	for _, app := range []store.App{a, b} {
		buildTestImage(t, nextImageTag(app), app.Name)
		var out strings.Builder
		if err := rollOut(s, app, node.Next, &out); err != nil {
			t.Fatalf("%v\n%s", err, out.String())
		}
	}
	a, _ = getApp(s, a.Name)
	b, _ = getApp(s, b.Name)
	bIP, err := deploy.ContainerIP(ctx(), b.ContainerName(), ProjectNetwork(b.ProjectKey()))
	if err != nil {
		t.Fatal(err)
	}
	// From a container on a's networks, what answers?
	reach := func(network, target string) bool {
		return exec.Command("docker", "run", "--rm", "--quiet", "--network", network, "busybox:1.36",
			"wget", "-q", "-T", "3", "-O", "/dev/null", "http://"+target+":8080/").Run() == nil
	}
	if !reach(ProjectNetwork(a.ProjectKey()), a.ContainerName()) {
		t.Error("an app isn't reachable in its own project")
	}
	for _, target := range []string{b.ContainerName(), bIP} {
		if reach(ProjectNetwork(a.ProjectKey()), target) {
			t.Errorf("another project's app is reachable at %s", target)
		}
	}
	if reach(projectEdge(a.ProjectKey()), EdgeAlias(b.Key())) {
		t.Error("another project's app is reachable on the edge network")
	}

	// Deleting a project removes its networks.
	if err := DeleteProject(s, b.ProjectID); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{ProjectNetwork(b.ProjectKey()), projectEdge(b.ProjectKey())} {
		if exec.Command("docker", "network", "inspect", n).Run() == nil {
			t.Errorf("network %s left after deleting its project", n)
		}
	}
	if err := DeleteProject(s, a.ProjectID); err != nil {
		t.Fatal(err)
	}
}

// TestDockerRestoreOnNewServer restores a database backup into a Postgres
// that has never seen the database, as after the panel was restored on a
// new server: the role and database are made first.
func TestDockerRestoreOnNewServer(t *testing.T) {
	if os.Getenv("HAKOBU_DOCKER_TEST") == "" {
		t.Skip("set HAKOBU_DOCKER_TEST=1 to run against the local Docker")
	}
	fakeR2(t)
	suffix, _ := RandomHex(3)
	old := node.PostgresContainer
	node.PostgresContainer = "zt-pg-" + suffix
	t.Cleanup(func() {
		_ = deploy.RemoveContainer(ctx(), node.PostgresContainer)
		_ = deploy.RemoveVolume(ctx(), node.PostgresContainer+"_data")
		node.PostgresContainer = old
	})
	dir := t.TempDir()
	t.Chdir(dir) // data/tmp
	s, err := store.Open(filepath.Join(dir, "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.SaveCloudflareToken(ctx(), "tok"))
	must(s.SaveCloudflareTunnel(ctx(), store.SaveCloudflareTunnelParams{AccountID: "acc", TunnelID: "t"}))
	must(SetupBackups(s))
	project := "zt" + suffix
	must(must2(CreateProject(s, 0, project)))
	pr, _ := getProject(s, project)
	t.Cleanup(func() { _ = removeProjectNetworks(s, pr) })
	must(must2(CreateDatabase(s, projectID(t, s, project), "zt"+suffix)))
	d, _ := getDB(s, "zt"+suffix)
	dockerOut(t, "exec", node.PostgresContainer, "psql", "-U", d.Key(), "-d", d.Key(), "-c", "CREATE TABLE t (n int); INSERT INTO t VALUES (7)")
	id, err := BackupDatabase(s, d.ID)
	must(err)

	// The new server's Postgres: nothing of this database or its role.
	must(deploy.PostgresExec(ctx(), node.PostgresContainer, `DROP DATABASE "`+d.Key()+`" WITH (FORCE)`))
	must(deploy.PostgresExec(ctx(), node.PostgresContainer, `DROP USER "`+d.Key()+`"`))
	b, err := s.GetBackup(ctx(), id)
	must(err)
	must(restoreBackup(s, b))
	if n := dockerOut(t, "exec", node.PostgresContainer, "psql", "-U", d.Key(), "-d", d.Key(), "-Atc", "SELECT n FROM t"); n != "7" {
		t.Errorf("restored row = %q", n)
	}
	// The recreated role logs in with the password apps were given.
	dockerOut(t, "exec", "-e", "PGPASSWORD="+string(d.Password), node.PostgresContainer, "psql", "-h", "127.0.0.1", "-U", d.Key(), "-d", d.Key(), "-c", "SELECT 1")
	must(DeleteDatabase(s, d.ID))
}

// TestDockerImportDump moves a database in from elsewhere: a dump owned by
// the superuser, with an extension, replaces a database that already has
// tables, and the app's role owns what it restored.
func TestDockerImportDump(t *testing.T) {
	if os.Getenv("HAKOBU_DOCKER_TEST") == "" {
		t.Skip("set HAKOBU_DOCKER_TEST=1 to run against the local Docker")
	}
	fakeR2(t)
	suffix, _ := RandomHex(3)
	old := node.PostgresContainer
	node.PostgresContainer = "zt-pg-" + suffix
	t.Cleanup(func() {
		_ = deploy.RemoveContainer(ctx(), node.PostgresContainer)
		_ = deploy.RemoveVolume(ctx(), node.PostgresContainer+"_data")
		node.PostgresContainer = old
	})
	dir := t.TempDir()
	t.Chdir(dir) // data/tmp
	s, err := store.Open(filepath.Join(dir, "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.SaveCloudflareToken(ctx(), "tok"))
	must(s.SaveCloudflareTunnel(ctx(), store.SaveCloudflareTunnelParams{AccountID: "acc", TunnelID: "t"}))
	must(SetupBackups(s))
	project := "zt" + suffix
	must(must2(CreateProject(s, 0, project)))
	pr, _ := getProject(s, project)
	t.Cleanup(func() { _ = removeProjectNetworks(s, pr) })
	must(must2(CreateDatabase(s, projectID(t, s, project), "zt"+suffix)))
	d, _ := getDB(s, "zt"+suffix)
	// The app already migrated an empty schema, as after a first deploy.
	dockerOut(t, "exec", node.PostgresContainer, "psql", "-U", d.Key(), "-d", d.Key(), "-c", "CREATE TABLE contacts (name text)")

	// The other host's database, made by its superuser.
	dockerOut(t, "exec", "-u", "postgres", node.PostgresContainer, "createdb", "-U", "postgres", "elsewhere")
	dockerOut(t, "exec", "-u", "postgres", node.PostgresContainer, "psql", "-U", "postgres", "-d", "elsewhere", "-c",
		"CREATE EXTENSION pg_trgm; CREATE TABLE contacts (name text); INSERT INTO contacts VALUES ('Kasl'); CREATE INDEX ON contacts USING gin (name gin_trgm_ops); GRANT SELECT ON contacts TO PUBLIC")
	dump, err := exec.Command("docker", "exec", "-u", "postgres", node.PostgresContainer, "pg_dump", "-Fc", "-U", "postgres", "-d", "elsewhere").Output()
	must(err)

	if _, err := ImportDump(s, d.ID, strings.NewReader("CREATE TABLE x;")); err == nil {
		t.Error("imported a plain SQL dump")
	}
	id, err := ImportDump(s, d.ID, bytes.NewReader(dump))
	must(err)
	must(VerifyBackup(s, id))
	b, err := s.GetBackup(ctx(), id)
	must(err)
	if b.Tables != 1 {
		t.Errorf("checked backup has %d tables", b.Tables)
	}
	must(restoreBackup(s, b))
	if n := dockerOut(t, "exec", node.PostgresContainer, "psql", "-U", d.Key(), "-d", d.Key(), "-Atc",
		"SELECT name FROM contacts WHERE name % 'Kas'"); n != "Kasl" {
		t.Errorf("restored rows = %q", n)
	}
	if owner := dockerOut(t, "exec", node.PostgresContainer, "psql", "-U", d.Key(), "-d", d.Key(), "-Atc",
		"SELECT tableowner FROM pg_tables WHERE tablename = 'contacts'"); owner != d.Key() {
		t.Errorf("contacts is owned by %q", owner)
	}
	// Restoring again replaces the data rather than clashing with it.
	dockerOut(t, "exec", node.PostgresContainer, "psql", "-U", d.Key(), "-d", d.Key(), "-c", "INSERT INTO contacts VALUES ('later')")
	must(restoreBackup(s, b))
	if n := dockerOut(t, "exec", node.PostgresContainer, "psql", "-U", d.Key(), "-d", d.Key(), "-Atc", "SELECT count(*) FROM contacts"); n != "1" {
		t.Errorf("after the second restore, %s rows", n)
	}
	// The renamed database still keeps other roles out.
	if out, err := exec.Command("docker", "exec", node.PostgresContainer, "psql", "-U", "postgres", "-d", d.Key(), "-Atc",
		"SELECT has_database_privilege('public', current_database(), 'CONNECT')").CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "f" {
		t.Errorf("PUBLIC may connect: %s %v", out, err)
	}
	must(DeleteDatabase(s, d.ID))
}

// TestDockerVolumeBackup backs a volume up while the app writes to it,
// changes the volume and restores the backup.
func TestDockerVolumeBackup(t *testing.T) {
	if os.Getenv("HAKOBU_DOCKER_TEST") == "" {
		t.Skip("set HAKOBU_DOCKER_TEST=1 to run against the local Docker")
	}
	objects, _ := fakeR2(t)
	dir := t.TempDir()
	t.Chdir(dir) // data/tmp
	s, err := store.Open(filepath.Join(dir, "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.SaveCloudflareToken(ctx(), "tok"))
	must(s.SaveCloudflareTunnel(ctx(), store.SaveCloudflareTunnelParams{AccountID: "acc"}))
	must(SetupBackups(s))
	app := newDockerTestApp(t, s)
	t.Cleanup(func() { _ = DeleteApp(s, app.ID) })
	must(AddVolume(s, app.ID, "data", "/data"))
	buildTestImage(t, nextImageTag(app), "v1")
	var out strings.Builder
	must(rollOut(s, app, node.Next, &out))
	must(promote(s, app))
	app, _ = getApp(s, app.Name)
	live := app.ContainerName()
	dockerOut(t, "exec", live, "sh", "-c", "echo secret-contents > /data/file && mkdir -p /data/sub && echo x > /data/sub/y && chown 1234:5678 /data/file && chmod 600 /data/file")

	id, err := BackupVolume(s, app.ID, "data")
	must(err)
	if st := dockerOut(t, "inspect", "-f", "{{.State.Status}}", live); st != "running" {
		t.Errorf("app is %s after the backup, want running", st)
	}
	for k, v := range objects {
		if strings.Contains(string(v), "secret-contents") {
			t.Errorf("%s holds the volume in the clear", k)
		}
	}
	must(VerifyVolumeBackup(s, id))
	b, _ := s.GetVolumeBackup(ctx(), id)
	if b.VerifyError != "" || b.Files < 3 { // log, file, sub/y
		t.Errorf("check: %d files, %q", b.Files, b.VerifyError)
	}

	dockerOut(t, "exec", live, "sh", "-c", "rm -r /data/sub && echo changed > /data/file && touch /data/new")
	must(StartVolumeRestore(s, app.ID, "data", id))
	waitIdle(t, app.ID)
	if j := VolumeJob(app.ID, "data"); j.Failed {
		t.Fatalf("restore: %s", j.Last)
	}
	got := dockerOut(t, "exec", live, "sh", "-c", "cat /data/file /data/sub/y; ls /data; stat -c '%u:%g %a' /data/file")
	if want := "secret-contents\nx\nfile\nlog\nsub\n1234:5678 600"; got != want {
		t.Errorf("after restore:\n%s\nwant:\n%s", got, want)
	}
	expectServing(t, app, "v1")
}

// checkStats: the live container's usage is read and recorded under the
// app's name.
func checkStats(t *testing.T, s *store.Store, app store.App) {
	t.Helper()
	running, err := deploy.RunningContainers(ctx())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range running {
		if c.Name != app.ContainerName() {
			continue
		}
		if target := containerTargets(s, server{})[c.Name]; target.name != AppTarget(app.ID) {
			t.Errorf("recorded as %q", target.name)
		}
		counters, err := deploy.ContainerStats(ctx(), c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if counters.Memory == 0 || counters.CPUNanos == 0 {
			t.Errorf("stats of the live container: %+v", counters)
		}
		return
	}
	t.Errorf("%s isn't among the running containers", app.ContainerName())
}

// TestDockerCache runs a real Valkey: reachable with its password from its
// project's network only, its data kept when it restarts with a new cap,
// and gone with it.
func TestDockerCache(t *testing.T) {
	if os.Getenv("HAKOBU_DOCKER_TEST") == "" {
		t.Skip("set HAKOBU_DOCKER_TEST=1 to run against the local Docker")
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	a, b := newDockerTestApp(t, s), newDockerTestApp(t, s)
	id, err := CreateCache(s, a.ProjectID, "")
	if err != nil {
		t.Fatal(err)
	}
	c, _ := s.GetCacheByID(ctx(), id)
	t.Cleanup(func() { _ = local.DropCache(ctx(), c.Key()) })
	cli := func(network, password string, args ...string) (string, error) {
		cmd := append([]string{"run", "--rm", "--network", network, config.ValkeyImage, "valkey-cli", "--no-auth-warning", "-h", c.Key(), "-a", password}, args...)
		out, err := exec.Command("docker", cmd...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	own := ProjectNetwork(a.ProjectKey())
	if out, err := cli(own, string(c.Password), "set", "k", "v"); err != nil || out != "OK" {
		t.Fatalf("set from its project: %q %v", out, err)
	}
	if out, _ := cli(own, "wrong", "get", "k"); strings.Contains(out, "v") && !strings.Contains(out, "WRONGPASS") {
		t.Errorf("read with a wrong password: %q", out)
	}
	if out, _ := cli(ProjectNetwork(b.ProjectKey()), string(c.Password), "get", "k"); out == "v" {
		t.Error("another project reaches the cache")
	}
	if err := SetCacheMemory(s, id, 64); err != nil {
		t.Fatal(err)
	}
	if out, err := cli(own, string(c.Password), "config", "get", "maxmemory"); err != nil || !strings.Contains(out, "67108864") {
		t.Errorf("maxmemory after the change: %q %v", out, err)
	}
	if out, err := cli(own, string(c.Password), "get", "k"); err != nil || out != "v" {
		t.Errorf("data after a restart: %q %v", out, err)
	}
	if err := DeleteCache(s, id); err != nil {
		t.Fatal(err)
	}
	if st, _ := deploy.ContainerStatus(ctx(), c.Key()); st != "not found" {
		t.Errorf("the deleted cache's container is %s", st)
	}
	if ok, _ := deploy.VolumeExists(ctx(), c.Key()+"_data"); ok {
		t.Error("the deleted cache's data is left")
	}
}

// TestDockerDataStatus asks a real Postgres and Valkey whether they
// answer, with a database's size, and counts a volume's bytes.
func TestDockerDataStatus(t *testing.T) {
	if os.Getenv("HAKOBU_DOCKER_TEST") == "" {
		t.Skip("set HAKOBU_DOCKER_TEST=1 to run against the local Docker")
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	app := newDockerTestApp(t, s)
	dbID, err := CreateDatabase(s, app.ProjectID, "ztstatus"+fmt.Sprint(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = DeleteDatabase(s, dbID) })
	cacheID, err := CreateCache(s, app.ProjectID, "")
	if err != nil {
		t.Fatal(err)
	}
	c, _ := s.GetCacheByID(ctx(), cacheID)
	t.Cleanup(func() { _ = local.DropCache(ctx(), c.Key()) })
	d, _ := s.GetDatabaseByID(ctx(), dbID)

	probes := []node.CacheProbe{{Name: c.Key(), Password: string(c.Password)}, {Name: c.Key(), Password: "wrong"}}
	st, err := local.DataStatus(ctx(), []string{d.Key(), "db_nope"}, probes[:1])
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Databases[d.Key()]; st.Postgres != "" || got.Missing || got.SizeBytes < 1<<20 {
		t.Errorf("Postgres %q, the database %+v", st.Postgres, got)
	}
	if !st.Databases["db_nope"].Missing {
		t.Error("a database Postgres hasn't wasn't missing")
	}
	if why, ok := st.Caches[c.Key()]; !ok || why != "" {
		t.Errorf("the cache: %q %v", why, ok)
	}
	st, _ = local.DataStatus(ctx(), nil, probes[1:])
	if st.Caches[c.Key()] != "doesn't answer PING" {
		t.Errorf("with a wrong password: %q", st.Caches[c.Key()])
	}

	if err := deploy.StopContainer(ctx(), c.Key()); err != nil {
		t.Fatal(err)
	}
	st, _ = local.DataStatus(ctx(), nil, probes[:1])
	if why := st.Caches[c.Key()]; !strings.HasPrefix(why, "isn't running") {
		t.Errorf("a stopped cache: %q", why)
	}

	vol := node.Volume(fmt.Sprintf("zt-%d", os.Getpid()), "data")
	if out, err := exec.Command("docker", "run", "--rm", "-v", vol+":/v", config.ValkeyImage, "sh", "-c", "head -c 300000 /dev/zero > /v/f").CombinedOutput(); err != nil {
		t.Fatalf("filling a volume: %v %s", err, out)
	}
	t.Cleanup(func() { _ = deploy.RemoveVolume(ctx(), vol) })
	sizes, err := local.VolumeSizes(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if n := sizes[vol]; n < 300000 {
		t.Errorf("the volume counts %d bytes, want 300000 or more", n)
	}
}
