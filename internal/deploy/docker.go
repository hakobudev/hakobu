package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var dockerClient = &http.Client{
	Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", "/var/run/docker.sock")
		},
	},
}

// pgLiteral renders a Postgres string literal with escaping.
func pgLiteral(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
}

func dockerRequest(ctx context.Context, method, path string, body interface{}) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reader = bytes.NewReader(b)
	}

	// хост в URL ігнорується (бо DialContext вище завжди йде в
	// unix socket), але net/http вимагає валідний URL все одно
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, reader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := dockerClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("docker daemon unreachable: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}

	return respBody, resp.StatusCode, nil
}

// RunContainerInternal starts a container on the hako docker network
// with no host port published — it's reachable from the host (and other
// containers) only via its network IP (see ContainerIP). This is how
// project app containers run now: the in-process proxy is the only thing
// that ever binds the project's public host port, so a new candidate
// container can be started and health-checked entirely off to the side of
// whatever is currently live.
func RunContainerInternal(ctx context.Context, imageTag, containerName string, containerPort int, env []string) (string, error) {
	if err := EnsureNetwork(ctx); err != nil {
		return "", fmt.Errorf("failed to ensure network: %w", err)
	}

	if err := removeIfExists(ctx, containerName); err != nil {
		return "", fmt.Errorf("failed to remove old container: %w", err)
	}

	portStr := fmt.Sprintf("%d/tcp", containerPort)

	createBody := map[string]interface{}{
		"Image": imageTag,
		"Env":   env,
		"ExposedPorts": map[string]interface{}{
			portStr: struct{}{},
		},
		"HostConfig": map[string]interface{}{
			"RestartPolicy": map[string]string{"Name": "unless-stopped"},
			"NetworkMode":   NetworkName,
		},
	}

	respBody, status, err := dockerRequest(ctx, "POST", "/containers/create?name="+containerName, createBody)
	if err != nil {
		return "", err
	}
	if status != http.StatusCreated {
		return "", fmt.Errorf("container create failed (%d): %s", status, string(respBody))
	}

	var created struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(respBody, &created); err != nil {
		return "", err
	}

	_, status, err = dockerRequest(ctx, "POST", "/containers/"+created.ID+"/start", nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusNoContent {
		return "", fmt.Errorf("container start failed with status %d", status)
	}

	return created.ID, nil
}

func removeIfExists(ctx context.Context, name string) error {
	return RemoveContainer(ctx, name)
}

// RunWorkerContainer starts a background worker from the same image a
// project's app builds — same repo/env access, but no exposed port and its
// own start command (run through a shell so users can type an ordinary
// command line) instead of the image's default CMD. There's no blue/green
// slot here: removing the old container before starting the new one is an
// acceptable brief gap for a non-traffic-serving process.
func RunWorkerContainer(ctx context.Context, imageTag, containerName, command string, env []string) (string, error) {
	if err := EnsureNetwork(ctx); err != nil {
		return "", fmt.Errorf("failed to ensure network: %w", err)
	}
	if err := removeIfExists(ctx, containerName); err != nil {
		return "", fmt.Errorf("failed to remove old container: %w", err)
	}

	createBody := map[string]interface{}{
		"Image": imageTag,
		"Env":   env,
		"HostConfig": map[string]interface{}{
			"RestartPolicy": map[string]string{"Name": "unless-stopped"},
			"NetworkMode":   NetworkName,
		},
	}
	if command != "" {
		createBody["Cmd"] = []string{"sh", "-c", command}
	}

	respBody, status, err := dockerRequest(ctx, "POST", "/containers/create?name="+containerName, createBody)
	if err != nil {
		return "", err
	}
	if status != http.StatusCreated {
		return "", fmt.Errorf("container create failed (%d): %s", status, string(respBody))
	}

	var created struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(respBody, &created); err != nil {
		return "", err
	}

	_, status, err = dockerRequest(ctx, "POST", "/containers/"+created.ID+"/start", nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusNoContent {
		return "", fmt.Errorf("container start failed with status %d", status)
	}

	return created.ID, nil
}

// RemoveContainer force-removes a container by name if it exists; a no-op
// if it doesn't (used both before recreating a container and for explicit
// project/database deletion).
func RemoveContainer(ctx context.Context, name string) error {
	respBody, status, err := dockerRequest(ctx, "GET", "/containers/json?all=true", nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("failed to list containers (%d): %s", status, string(respBody))
	}

	var containers []struct {
		ID    string   `json:"Id"`
		Names []string `json:"Names"`
	}
	if err := json.Unmarshal(respBody, &containers); err != nil {
		return err
	}

	for _, c := range containers {
		for _, n := range c.Names {
			if n == "/"+name {
				_, status, err := dockerRequest(ctx, "DELETE", "/containers/"+c.ID+"?force=true", nil)
				if err != nil {
					return err
				}
				if status != http.StatusNoContent {
					return fmt.Errorf("failed to remove container (%d)", status)
				}
				return nil
			}
		}
	}
	return nil
}

func pullImageIfMissing(ctx context.Context, image string) error {
	_, status, err := dockerRequest(ctx, "GET", "/images/"+url.PathEscape(image)+"/json", nil)
	if err != nil {
		return err
	}
	if status == http.StatusOK {
		return nil
	}

	respBody, status, err := dockerRequest(ctx, "POST", "/images/create?fromImage="+url.QueryEscape(image), nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("image pull failed (%d): %s", status, string(respBody))
	}
	return nil
}

// HTTPCheck performs a short GET against the URL and reports whether
// anything answered at all (any status code counts as "alive" — the app
// itself may legitimately 4xx/5xx a bare "/").
func HTTPCheck(url string) bool {
	up, _ := TimedHTTPCheck(url, false)
	return up
}

// HealthcheckHeader marks a request as hako's own synthetic health
// check rather than real traffic. Checks against a project's public port go
// through the same reverse proxy real visitors do (proving the proxy
// itself is up, not just the backend) — without this, the traffic pulse
// dashboard shows would treat our own polling as activity, keeping the
// "live" indicator lit even with zero real requests.
const HealthcheckHeader = "X-Hako-Healthcheck"

// TimedHTTPCheck is HTTPCheck plus the response time in milliseconds, for
// uptime-style dashboards. requireOK controls whether a non-2xx response
// counts as down: false treats any response as "alive" (the right default
// for a bare "/" the app may never have handled), true — for a
// purpose-built health endpoint a caller explicitly configured — actually
// checks the status code, so a readiness probe correctly reporting 503
// (e.g. its database is unreachable) doesn't get treated as healthy just
// because something answered.
func TimedHTTPCheck(url string, requireOK bool) (bool, int) {
	client := &http.Client{Timeout: 3 * time.Second}
	start := time.Now()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return false, 0
	}
	req.Header.Set(HealthcheckHeader, "1")
	resp, err := client.Do(req)
	elapsed := int(time.Since(start).Milliseconds())
	if err != nil {
		return false, elapsed
	}
	defer resp.Body.Close()
	if requireOK && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
		return false, elapsed
	}
	return true, elapsed
}

// WaitHealthy polls url until it responds or attempts are exhausted — the
// gate a freshly deployed candidate container must clear before it's
// allowed to take over from the previous, known-good one. See
// TimedHTTPCheck for what requireOK changes.
func WaitHealthy(url string, attempts int, interval time.Duration, requireOK bool) bool {
	return WaitHealthyFunc(attempts, interval, func() bool {
		up, _ := TimedHTTPCheck(url, requireOK)
		return up
	})
}

// WaitHealthyFunc is WaitHealthy's non-HTTP counterpart, polling an
// arbitrary check instead — used for services with no HTTP endpoint to
// probe, like the shared Postgres service (checked via pg_isready).
func WaitHealthyFunc(attempts int, interval time.Duration, check func() bool) bool {
	for i := 0; i < attempts; i++ {
		if check() {
			return true
		}
		time.Sleep(interval)
	}
	return false
}

// execInContainer runs cmd inside containerName via the Docker exec API and
// returns its combined stdout+stderr plus exit code — the shared plumbing
// behind PostgresReady, disk-usage and connection-count checks.
func execInContainer(ctx context.Context, containerName string, cmd []string) (output string, exitCode int, err error) {
	createBody := map[string]interface{}{
		"Cmd":          cmd,
		"AttachStdout": true,
		"AttachStderr": true,
	}
	respBody, status, err := dockerRequest(ctx, "POST", "/containers/"+containerName+"/exec", createBody)
	if err != nil {
		return "", 0, err
	}
	if status != http.StatusCreated {
		return "", 0, fmt.Errorf("exec create failed (%d): %s", status, string(respBody))
	}

	var created struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(respBody, &created); err != nil {
		return "", 0, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "http://docker/exec/"+created.ID+"/start", bytes.NewReader([]byte(`{"Detach":false,"Tty":false}`)))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := dockerClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("docker daemon unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", 0, fmt.Errorf("exec start failed (%d): %s", resp.StatusCode, string(body))
	}

	var out bytes.Buffer
	header := make([]byte, 8)
	for {
		if _, err := io.ReadFull(resp.Body, header); err != nil {
			break
		}
		size := int(header[4])<<24 | int(header[5])<<16 | int(header[6])<<8 | int(header[7])
		if _, err := io.CopyN(&out, resp.Body, int64(size)); err != nil {
			break
		}
	}

	inspectBody, status, err := dockerRequest(ctx, "GET", "/exec/"+created.ID+"/json", nil)
	if err != nil {
		return out.String(), 0, err
	}
	if status != http.StatusOK {
		return out.String(), 0, fmt.Errorf("exec inspect failed (%d): %s", status, string(inspectBody))
	}

	var inspect struct {
		ExitCode int `json:"ExitCode"`
	}
	if err := json.Unmarshal(inspectBody, &inspect); err != nil {
		return out.String(), 0, err
	}
	return out.String(), inspect.ExitCode, nil
}

// PostgresReady runs `pg_isready` inside the database container via the
// Docker exec API and reports whether it exited 0 (accepting connections).
func PostgresReady(ctx context.Context, containerName string) (bool, error) {
	_, exitCode, err := execInContainer(ctx, containerName, []string{"pg_isready", "-U", "postgres"})
	if err != nil {
		return false, err
	}
	return exitCode == 0, nil
}

// PostgresExec runs one SQL statement inside the shared Postgres service as
// the postgres superuser — local trust auth via `docker exec` means no
// password is ever needed here, the mechanism ops.CreateDatabase/
// DeleteDatabase use to provision or tear down one project's own logical
// database and role without touching any other project's.
func PostgresExec(ctx context.Context, containerName, sql string) error {
	out, exitCode, err := execInContainer(ctx, containerName, []string{"psql", "-U", "postgres", "-c", sql})
	if err != nil {
		return err
	}
	if exitCode != 0 {
		return fmt.Errorf("psql exited %d: %s", exitCode, strings.TrimSpace(out))
	}
	return nil
}

// PostgresDatabaseSizeMB returns one logical database's own size (not the
// whole data directory — containerName is now hako's one shared
// Postgres service, hosting every project's database, so a directory-wide
// `du` would report the same combined number for every project).
func PostgresDatabaseSizeMB(ctx context.Context, containerName, dbName string) (float64, error) {
	out, exitCode, err := execInContainer(ctx, containerName, []string{
		"psql", "-U", "postgres", "-tAc", fmt.Sprintf("SELECT pg_database_size(%s) / 1048576.0", pgLiteral(dbName)),
	})
	if err != nil {
		return 0, err
	}
	if exitCode != 0 {
		return 0, fmt.Errorf("psql exited %d: %s", exitCode, strings.TrimSpace(out))
	}
	mb, err := strconv.ParseFloat(strings.TrimSpace(out), 64)
	if err != nil {
		return 0, fmt.Errorf("unexpected psql output: %q", out)
	}
	return mb, nil
}

// PostgresConnectionCount returns the number of active connections
// (pg_stat_activity rows) to one specific database — scoped, since
// containerName is now a shared service where an unscoped count would mix
// in every other project's connections too.
func PostgresConnectionCount(ctx context.Context, containerName, dbName string) (int, error) {
	out, exitCode, err := execInContainer(ctx, containerName, []string{
		"psql", "-U", "postgres", "-tAc", fmt.Sprintf("SELECT count(*) FROM pg_stat_activity WHERE datname = %s", pgLiteral(dbName)),
	})
	if err != nil {
		return 0, err
	}
	if exitCode != 0 {
		return 0, fmt.Errorf("psql exited %d: %s", exitCode, strings.TrimSpace(out))
	}
	count, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("unexpected psql output: %q", out)
	}
	return count, nil
}

// ContainerCounts returns how many docker containers exist total and how
// many are currently running, across the whole host (not just hako's
// own), for a quick "is this box under control" glance.
func ContainerCounts(ctx context.Context) (running, total int, err error) {
	respBody, status, err := dockerRequest(ctx, "GET", "/containers/json?all=true", nil)
	if err != nil {
		return 0, 0, err
	}
	if status != http.StatusOK {
		return 0, 0, fmt.Errorf("failed to list containers (%d): %s", status, string(respBody))
	}

	var containers []struct {
		State string `json:"State"`
	}
	if err := json.Unmarshal(respBody, &containers); err != nil {
		return 0, 0, err
	}

	total = len(containers)
	for _, c := range containers {
		if c.State == "running" {
			running++
		}
	}
	return running, total, nil
}

// ContainerLogs returns the last tailLines of stdout+stderr from the
// container — the same output `docker logs` shows, which is where the app
// itself reports its own errors (stack traces, DB failures, etc.), a level
// of detail HTTP status codes alone don't give you.
func ContainerLogs(ctx context.Context, containerName string, tailLines int) (string, error) {
	path := fmt.Sprintf("/containers/%s/logs?stdout=1&stderr=1&tail=%d&timestamps=1", containerName, tailLines)

	req, err := http.NewRequestWithContext(ctx, "GET", "http://docker"+path, nil)
	if err != nil {
		return "", err
	}
	resp, err := dockerClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("docker daemon unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("logs failed (%d): %s", resp.StatusCode, string(body))
	}

	// Docker multiplexes stdout/stderr with an 8-byte frame header
	// (1-byte stream type, 3 reserved, 4-byte big-endian length) unless
	// the container was started with a TTY — ours aren't.
	var out bytes.Buffer
	header := make([]byte, 8)
	for {
		if _, err := io.ReadFull(resp.Body, header); err != nil {
			break
		}
		size := int(header[4])<<24 | int(header[5])<<16 | int(header[6])<<8 | int(header[7])
		if _, err := io.CopyN(&out, resp.Body, int64(size)); err != nil {
			break
		}
	}
	return out.String(), nil
}

// ContainerStatus returns docker's status string for the container (e.g.
// "running", "restarting", "exited"), or "not found" if it doesn't exist.
// ContainerIP returns the container's IP address on the hako docker
// network — reachable directly from the host, since Docker's user-defined
// bridge networks route from the host without needing a published port.
func ContainerIP(ctx context.Context, containerName string) (string, error) {
	respBody, status, err := dockerRequest(ctx, "GET", "/containers/"+containerName+"/json", nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("container inspect failed (%d): %s", status, string(respBody))
	}

	var info struct {
		NetworkSettings struct {
			Networks map[string]struct {
				IPAddress string `json:"IPAddress"`
			} `json:"Networks"`
		} `json:"NetworkSettings"`
	}
	if err := json.Unmarshal(respBody, &info); err != nil {
		return "", err
	}

	net, ok := info.NetworkSettings.Networks[NetworkName]
	if !ok || net.IPAddress == "" {
		return "", fmt.Errorf("container %q has no IP on network %q", containerName, NetworkName)
	}
	return net.IPAddress, nil
}

// ContainerStatus reports docker's own state string for containerName
// (running, exited, ...) along with how many times docker has restarted it
// since creation — a crash-looping app still shows as "running" between
// crashes, so the restart count is what actually surfaces the flapping.
func ContainerStatus(ctx context.Context, containerName string) (status string, restarts int, err error) {
	respBody, code, err := dockerRequest(ctx, "GET", "/containers/"+containerName+"/json", nil)
	if err != nil {
		return "", 0, err
	}
	if code == http.StatusNotFound {
		return "not found", 0, nil
	}
	if code != http.StatusOK {
		return "", 0, fmt.Errorf("container inspect failed (%d): %s", code, string(respBody))
	}

	var info struct {
		State struct {
			Status string `json:"Status"`
		} `json:"State"`
		RestartCount int `json:"RestartCount"`
	}
	if err := json.Unmarshal(respBody, &info); err != nil {
		return "", 0, err
	}
	return info.State.Status, info.RestartCount, nil
}

type ContainerStats struct {
	CPUPercent float64
	MemUsedMB  float64
	MemLimitMB float64
}

// GetContainerStats takes a single (non-streaming) snapshot of docker's
// resource stats and computes CPU%/memory the same way `docker stats` does.
func GetContainerStats(ctx context.Context, containerName string) (ContainerStats, error) {
	respBody, status, err := dockerRequest(ctx, "GET", "/containers/"+containerName+"/stats?stream=false", nil)
	if err != nil {
		return ContainerStats{}, err
	}
	if status != http.StatusOK {
		return ContainerStats{}, fmt.Errorf("stats failed (%d): %s", status, string(respBody))
	}

	var raw struct {
		CPUStats struct {
			CPUUsage struct {
				TotalUsage uint64 `json:"total_usage"`
			} `json:"cpu_usage"`
			SystemCPUUsage uint64 `json:"system_cpu_usage"`
			OnlineCPUs     uint64 `json:"online_cpus"`
		} `json:"cpu_stats"`
		PreCPUStats struct {
			CPUUsage struct {
				TotalUsage uint64 `json:"total_usage"`
			} `json:"cpu_usage"`
			SystemCPUUsage uint64 `json:"system_cpu_usage"`
		} `json:"precpu_stats"`
		MemoryStats struct {
			Usage uint64 `json:"usage"`
			Limit uint64 `json:"limit"`
		} `json:"memory_stats"`
	}
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return ContainerStats{}, err
	}

	cpuDelta := float64(raw.CPUStats.CPUUsage.TotalUsage) - float64(raw.PreCPUStats.CPUUsage.TotalUsage)
	systemDelta := float64(raw.CPUStats.SystemCPUUsage) - float64(raw.PreCPUStats.SystemCPUUsage)
	onlineCPUs := float64(raw.CPUStats.OnlineCPUs)
	if onlineCPUs == 0 {
		onlineCPUs = 1
	}

	var cpuPercent float64
	if systemDelta > 0 && cpuDelta > 0 {
		cpuPercent = (cpuDelta / systemDelta) * onlineCPUs * 100
	}

	const mb = 1024 * 1024
	return ContainerStats{
		CPUPercent: cpuPercent,
		MemUsedMB:  float64(raw.MemoryStats.Usage) / mb,
		MemLimitMB: float64(raw.MemoryStats.Limit) / mb,
	}, nil
}

// RunServiceContainer starts a long-lived, singleton backing service
// (the shared Postgres service, a self-hosted RustFS instance, ...) on
// hako's default network with a persistent named volume — unlike
// RunContainerInternal/RunWorkerContainer, this is for infrastructure
// hako itself manages, not a deployed project.
func RunServiceContainer(ctx context.Context, containerName, image string, env []string, volumeName, mountPath string) (string, error) {
	if err := EnsureNetwork(ctx); err != nil {
		return "", err
	}

	if err := pullImageIfMissing(ctx, image); err != nil {
		return "", fmt.Errorf("failed to pull image %q: %w", image, err)
	}

	if err := removeIfExists(ctx, containerName); err != nil {
		return "", err
	}

	createBody := map[string]interface{}{
		"Image": image,
		"Env":   env,
		"HostConfig": map[string]interface{}{
			"NetworkMode":   NetworkName,
			"Binds":         []string{volumeName + "_data:" + mountPath},
			"RestartPolicy": map[string]string{"Name": "unless-stopped"},
		},
	}

	respBody, status, err := dockerRequest(ctx, "POST", "/containers/create?name="+containerName, createBody)
	if err != nil {
		return "", err
	}
	if status != http.StatusCreated {
		return "", fmt.Errorf("db container create failed (%d): %s", status, string(respBody))
	}

	var created struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(respBody, &created); err != nil {
		return "", err
	}

	_, status, err = dockerRequest(ctx, "POST", "/containers/"+created.ID+"/start", nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusNoContent {
		return "", fmt.Errorf("db container start failed with status %d", status)
	}

	return created.ID, nil
}

// splitImageRef splits "repo:tag" into its two parts, defaulting to
// "latest" the same way Docker itself does when a reference has no tag.
func splitImageRef(ref string) (repo, tag string) {
	i := strings.LastIndex(ref, ":")
	if i == -1 {
		return ref, "latest"
	}
	return ref[:i], ref[i+1:]
}

// TagImage points targetRef at whatever image sourceRef currently
// resolves to — no image data is copied, just a new (or moved) tag. Used
// to snapshot the image a fresh build is about to overwrite, so a rollback
// has something to redeploy afterwards.
func TagImage(ctx context.Context, sourceRef, targetRef string) error {
	repo, tag := splitImageRef(targetRef)
	respBody, status, err := dockerRequest(ctx, "POST", "/images/"+url.PathEscape(sourceRef)+"/tag?repo="+url.QueryEscape(repo)+"&tag="+url.QueryEscape(tag), nil)
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return fmt.Errorf("image tag failed (%d): %s", status, string(respBody))
	}
	return nil
}

// ImageExists reports whether ref resolves to a locally known image —
// used to skip snapshotting a "previous" tag before a project's first-ever
// build, and to give rollback a clear "nothing to roll back to" error.
func ImageExists(ctx context.Context, ref string) (bool, error) {
	_, status, err := dockerRequest(ctx, "GET", "/images/"+url.PathEscape(ref)+"/json", nil)
	if err != nil {
		return false, err
	}
	if status == http.StatusNotFound {
		return false, nil
	}
	if status != http.StatusOK {
		return false, fmt.Errorf("image inspect failed (%d)", status)
	}
	return true, nil
}
