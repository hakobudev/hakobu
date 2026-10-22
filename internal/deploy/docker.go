// Package deploy talks to the Docker Engine API over its unix socket.
package deploy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// NetworkName is the home network of hakobu's services (Postgres),
// EdgeNetwork cloudflared's. Apps live on their project's networks (see
// ops), which the services and cloudflared join as needed. Tests use
// networks of their own.
var (
	NetworkName = "hakobu"
	EdgeNetwork = "hakobu-edge"
)

// dockerSocket is the daemon's socket: DOCKER_HOST (unix:// only) when set,
// as under rootless Docker, else the rootful default.
func dockerSocket() string {
	if p, ok := strings.CutPrefix(os.Getenv("DOCKER_HOST"), "unix://"); ok && p != "" {
		return p
	}
	return "/var/run/docker.sock"
}

var dockerClient = &http.Client{
	Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "unix", dockerSocket())
		},
	},
}

// withDeadline bounds a Docker call whose caller set no deadline (most pass
// context.Background()): a hung daemon mustn't hold a deploy, a backup or
// the health checks forever. No timeout on the client itself: the event
// stream stays open for days.
func withDeadline(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}

// requestTimeout is how long an API call may take: pulls and prunes can
// take a while, everything else answers in seconds.
func requestTimeout(path string) time.Duration {
	if strings.HasPrefix(path, "/images/create") || strings.HasPrefix(path, "/build/prune") || strings.HasSuffix(path, "/wait") {
		return 30 * time.Minute
	}
	return 2 * time.Minute
}

// maxResponse caps what hakobu reads from one API answer.
const maxResponse = 64 << 20

func dockerRequest(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	ctx, cancel := withDeadline(ctx, requestTimeout(path))
	defer cancel()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reader = bytes.NewReader(b)
	}
	// The host is ignored: the transport always dials the unix socket.
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
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	return respBody, resp.StatusCode, err
}

// EnsureNetwork creates a bridge network unless it exists.
func EnsureNetwork(ctx context.Context, name string) error {
	return ensureNetwork(ctx, name)
}

func ensureNetwork(ctx context.Context, name string) error {
	if _, status, err := dockerRequest(ctx, "GET", "/networks/"+name, nil); err == nil && status == http.StatusOK {
		return nil
	}
	respBody, status, err := dockerRequest(ctx, "POST", "/networks/create", map[string]any{"Name": name, "Driver": "bridge"})
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return fmt.Errorf("network create failed (%d): %s", status, respBody)
	}
	return nil
}

// logConfig caps what Docker keeps of a container's output: without it a
// log grows until the disk is full, and a line cut short there leaves the
// rest of the log unreadable.
var logConfig = map[string]any{"Type": "json-file", "Config": map[string]string{"max-size": "10m", "max-file": "3"}}

// runContainer replaces any container with the same name and starts a new
// one on the hakobu network (or hostConfig's NetworkMode), with
// restart=unless-stopped unless hostConfig sets another restart policy, and
// its output capped (logConfig).
func runContainer(ctx context.Context, name string, spec map[string]any, hostConfig map[string]any) (string, error) {
	if n, _ := hostConfig["NetworkMode"].(string); n == "" {
		hostConfig["NetworkMode"] = NetworkName
	}
	if err := ensureNetwork(ctx, hostConfig["NetworkMode"].(string)); err != nil {
		return "", fmt.Errorf("failed to ensure network: %w", err)
	}
	if err := RemoveContainer(ctx, name); err != nil {
		return "", fmt.Errorf("failed to remove old container: %w", err)
	}
	if hostConfig["RestartPolicy"] == nil {
		hostConfig["RestartPolicy"] = map[string]string{"Name": "unless-stopped"}
	}
	hostConfig["LogConfig"] = logConfig
	spec["HostConfig"] = hostConfig

	respBody, status, err := dockerRequest(ctx, "POST", "/containers/create?name="+url.QueryEscape(name), spec)
	if err != nil {
		return "", err
	}
	if status != http.StatusCreated {
		return "", fmt.Errorf("container create failed (%d): %s", status, respBody)
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(respBody, &created); err != nil {
		return "", err
	}
	respBody, status, err = dockerRequest(ctx, "POST", "/containers/"+created.ID+"/start", nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusNoContent {
		return "", fmt.Errorf("container start failed (%d): %s", status, respBody)
	}
	return created.ID, nil
}

// AppLabel names the app an app or worker container belongs to.
const AppLabel = "hakobu.app"

// appCapDrop are the capabilities app and worker containers go without.
var appCapDrop = []string{"NET_RAW", "MKNOD", "AUDIT_WRITE"}

// AppOptions are what an app's containers (app and worker) have in common.
type AppOptions struct {
	App      string
	Network  string // the project's network
	Env      []string
	Binds    []string
	MemoryMB int64   // 0: no limit
	CPUs     float64 // 0: no limit
	// Command is a shell command run instead of the image's own start
	// ("" keeps it).
	Command string
}

func (o AppOptions) spec(imageTag string) (spec, hostConfig map[string]any) {
	spec = map[string]any{"Image": imageTag, "Env": o.Env, "Labels": map[string]string{AppLabel: o.App}}
	if o.Command != "" {
		// The entrypoint goes too: Railpack's images start with
		// ["/bin/bash", "-c"], which would take a Cmd of ["sh", "-c",
		// command] for a script that only says "sh".
		spec["Entrypoint"] = []string{"/bin/sh", "-c"}
		spec["Cmd"] = []string{o.Command}
	}
	hostConfig = map[string]any{
		"NetworkMode": o.Network,
		"Binds":       o.Binds,
		// setuid binaries can't raise privileges inside the container.
		"SecurityOpt": []string{"no-new-privileges"},
		// Apps share their project's network with Postgres and
		// cloudflared: without raw sockets an app can't spoof ARP or DNS
		// on it. The rest of Docker's defaults stay, for images that start
		// as root and switch users.
		"CapDrop": appCapDrop,
	}
	if o.MemoryMB > 0 {
		hostConfig["Memory"] = o.MemoryMB << 20
		hostConfig["MemorySwap"] = o.MemoryMB << 20 // no swap on top
	}
	if o.CPUs > 0 {
		hostConfig["NanoCpus"] = int64(o.CPUs * 1e9)
	}
	return spec, hostConfig
}

// RunAppContainer starts an app container with no host port published; the
// in-process proxy reaches it by its network IP. It isn't restarted until
// KeepRestarting: a candidate that crashes stays exited, with its exit
// reason, for the health check to see.
func RunAppContainer(ctx context.Context, imageTag, name string, opts AppOptions) (string, error) {
	spec, hostConfig := opts.spec(imageTag)
	hostConfig["RestartPolicy"] = map[string]string{"Name": "no"}
	return runContainer(ctx, name, spec, hostConfig)
}

// KeepRestarting has Docker restart the container whenever it stops, until
// it's stopped on purpose.
func KeepRestarting(ctx context.Context, name string) error {
	respBody, status, err := dockerRequest(ctx, "POST", "/containers/"+url.PathEscape(name)+"/update",
		map[string]any{"RestartPolicy": map[string]string{"Name": "unless-stopped"}})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("container update failed (%d): %s", status, respBody)
	}
	return nil
}

// RunWorkerContainer starts a worker from the app's image with a shell command.
func RunWorkerContainer(ctx context.Context, imageTag, name, command string, opts AppOptions) (string, error) {
	if command != "" {
		opts.Command = command
	}
	spec, hostConfig := opts.spec(imageTag)
	return runContainer(ctx, name, spec, hostConfig)
}

// RunServiceContainer starts a hakobu-managed backing service (Postgres)
// with a persistent named volume and shmSize bytes of /dev/shm (0:
// Docker's 64 MB).
func RunServiceContainer(ctx context.Context, name, image string, env []string, mountPath string, shmSize int64) (string, error) {
	if err := pullImageIfMissing(ctx, image); err != nil {
		return "", fmt.Errorf("failed to pull image %q: %w", image, err)
	}
	host := map[string]any{"Binds": []string{name + "_data:" + mountPath}}
	if shmSize > 0 {
		host["ShmSize"] = shmSize
	}
	return runContainer(ctx, name, map[string]any{"Image": image, "Env": env}, host)
}

// RunCacheContainer runs a Valkey server for one cache on network, its
// data in the volume <name>_data, capped at memoryMB: Valkey evicts
// nothing at the cap (a queue mustn't lose jobs) but refuses writes, and
// the container's own limit leaves it room for its overhead. stamp is kept
// in its environment as HAKOBU_CACHE, to tell whether it runs as wanted.
func RunCacheContainer(ctx context.Context, name, image, password string, memoryMB int64, network, stamp string) (string, error) {
	if err := pullImageIfMissing(ctx, image); err != nil {
		return "", fmt.Errorf("failed to pull image %q: %w", image, err)
	}
	limit := memoryMB + memoryMB/4 + 32
	return runContainer(ctx, name, map[string]any{
		"Image": image,
		"Env":   []string{"HAKOBU_CACHE=" + stamp},
		"Cmd": []string{"valkey-server", "--requirepass", password, "--appendonly", "yes",
			"--maxmemory", fmt.Sprintf("%dmb", memoryMB), "--maxmemory-policy", "noeviction"},
	}, map[string]any{
		"NetworkMode": network,
		"Binds":       []string{name + "_data:/data"},
		"SecurityOpt": []string{"no-new-privileges"},
		"CapDrop":     appCapDrop,
		"Memory":      limit << 20,
		"MemorySwap":  limit << 20,
	})
}

// ValkeyReady reports whether the cache's Valkey answers PING.
func ValkeyReady(ctx context.Context, name, password string) bool {
	out, code, err := execInContainer(ctx, name, []string{"valkey-cli", "--no-auth-warning", "-a", password, "ping"})
	return err == nil && code == 0 && strings.TrimSpace(out) == "PONG"
}

// ConnectNetwork adds a container to another network, with aliases if
// given; a no-op if it's on it already. Joining a network leaves the
// container's connections on its other networks alone.
func ConnectNetwork(ctx context.Context, container, network string, aliases ...string) error {
	info, err := inspect(ctx, container)
	if err != nil {
		return err
	}
	if info == nil {
		return fmt.Errorf("container %q not found", container)
	}
	if _, ok := info.NetworkSettings.Networks[network]; ok {
		return nil
	}
	if err := ensureNetwork(ctx, network); err != nil {
		return err
	}
	respBody, status, err := dockerRequest(ctx, "POST", "/networks/"+url.PathEscape(network)+"/connect", map[string]any{
		"Container": container, "EndpointConfig": map[string]any{"Aliases": aliases},
	})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		if info.State.Status != "running" {
			return fmt.Errorf("%w: connecting %s to %s: %s", ErrNotRunning, container, network, respBody)
		}
		return fmt.Errorf("connecting %s to %s failed (%d): %s", container, network, status, respBody)
	}
	return nil
}

// ErrStaleNetwork: Docker won't take a container that isn't running (one
// restarting again and again, say) off a network its configuration still
// lists, and it would join that network again at its next start; only
// recreating the container drops it.
var ErrStaleNetwork = errors.New("container can't leave a network it isn't running on")

// ErrNotRunning: Docker can't connect a container that isn't running (one
// restarting again and again, say) to a network.
var ErrNotRunning = errors.New("container isn't running")

// RemoveNetwork deletes a network; a missing one is fine.
func RemoveNetwork(ctx context.Context, name string) error {
	respBody, status, err := dockerRequest(ctx, "DELETE", "/networks/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusNoContent, http.StatusOK, http.StatusNotFound:
		return nil
	}
	return fmt.Errorf("removing network %s failed (%d): %s", name, status, respBody)
}

// StopContainer stops a container but keeps it, so it can be started again;
// a no-op if it doesn't exist or isn't running.
func StopContainer(ctx context.Context, name string) error {
	return stopContainer(ctx, name, 10*time.Second)
}

// stopContainer gives the container wait to exit before it's killed.
func stopContainer(ctx context.Context, name string, wait time.Duration) error {
	ctx, cancel := withDeadline(ctx, wait+time.Minute)
	defer cancel()
	respBody, status, err := dockerRequest(ctx, "POST", fmt.Sprintf("/containers/%s/stop?t=%d", url.PathEscape(name), int(wait.Seconds())), nil)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusNoContent, http.StatusNotModified, http.StatusNotFound:
		return nil
	}
	return fmt.Errorf("failed to stop container %s (%d): %s", name, status, respBody)
}

// RemoveVolume deletes a volume; in-use and missing volumes are left alone.
func RemoveVolume(ctx context.Context, name string) error {
	respBody, status, err := dockerRequest(ctx, "DELETE", "/volumes/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusNoContent, http.StatusNotFound, http.StatusConflict:
		return nil
	}
	return fmt.Errorf("failed to remove volume %s (%d): %s", name, status, respBody)
}

// RunningWithVolume lists the running containers that mount volume.
func RunningWithVolume(ctx context.Context, volume string) ([]string, error) {
	filters, _ := json.Marshal(map[string][]string{"volume": {volume}, "status": {"running"}})
	respBody, status, err := dockerRequest(ctx, "GET", "/containers/json?filters="+url.QueryEscape(string(filters)), nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("container list failed (%d): %s", status, respBody)
	}
	var list []struct{ Names []string }
	if err := json.Unmarshal(respBody, &list); err != nil {
		return nil, err
	}
	var names []string
	for _, c := range list {
		if len(c.Names) > 0 {
			names = append(names, strings.TrimPrefix(c.Names[0], "/"))
		}
	}
	return names, nil
}

// PauseContainer freezes a container's processes; UnpauseContainer lets
// them go on where they were.
func PauseContainer(ctx context.Context, name string) error {
	return pauseOrUnpause(ctx, name, "pause")
}

func UnpauseContainer(ctx context.Context, name string) error {
	return pauseOrUnpause(ctx, name, "unpause")
}

func pauseOrUnpause(ctx context.Context, name, action string) error {
	respBody, status, err := dockerRequest(ctx, "POST", "/containers/"+url.PathEscape(name)+"/"+action, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return fmt.Errorf("%s of %s failed (%d): %s", action, name, status, respBody)
	}
	return nil
}

// RemoveContainer force-removes a container; a no-op if it doesn't exist.
func RemoveContainer(ctx context.Context, name string) error {
	respBody, status, err := dockerRequest(ctx, "DELETE", "/containers/"+url.PathEscape(name)+"?force=true", nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent && status != http.StatusNotFound {
		return fmt.Errorf("failed to remove container %s (%d): %s", name, status, respBody)
	}
	return nil
}

func pullImageIfMissing(ctx context.Context, image string) error {
	if ok, err := ImageExists(ctx, image); err != nil || ok {
		return err
	}
	respBody, status, err := dockerRequest(ctx, "POST", "/images/create?fromImage="+url.QueryEscape(image), nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("image pull failed (%d): %s", status, respBody)
	}
	return nil
}

// healthTransport keeps no connections: each check reaches a fresh candidate.
var healthTransport = func() *http.Transport {
	t := newContainerTransport()
	t.DisableKeepAlives = true
	return t
}()

// HTTPCheck GETs url. With requireOK, only a 2xx counts as healthy; without
// it any response does (the app may never have handled a bare "/").
func HTTPCheck(url string, requireOK bool) bool {
	client := &http.Client{
		Timeout: 3 * time.Second,
		// The app answers for itself; following its redirects would let it
		// point hakobu at anything reachable from the host.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     healthTransport,
	}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "hakobu-health-check")
	// A trace marked not sampled: Sentry SDKs follow the caller's decision,
	// so the checks, every 30 s, don't fill the app's traces.
	req.Header.Set("sentry-trace", unsampledTrace())
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return !requireOK || (resp.StatusCode >= 200 && resp.StatusCode < 300)
}

// unsampledTrace is a sentry-trace header value, trace-span-sampled, for a
// new trace that isn't sampled.
func unsampledTrace() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b[:16]) + "-" + hex.EncodeToString(b[16:]) + "-0"
}

func WaitHealthy(attempts int, interval time.Duration, check func() bool) bool {
	for i := 0; i < attempts; i++ {
		if check() {
			return true
		}
		time.Sleep(interval)
	}
	return false
}

// demux strips Docker's 8-byte stream frame headers from non-TTY output.
func demux(r io.Reader) string {
	var out bytes.Buffer
	header := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, header); err != nil {
			break
		}
		size := int64(header[4])<<24 | int64(header[5])<<16 | int64(header[6])<<8 | int64(header[7])
		if _, err := io.CopyN(&out, r, size); err != nil {
			break
		}
	}
	return out.String()
}

func execInContainer(ctx context.Context, containerName string, cmd []string) (output string, exitCode int, err error) {
	ctx, cancel := withDeadline(ctx, 30*time.Minute)
	defer cancel()
	respBody, status, err := dockerRequest(ctx, "POST", "/containers/"+containerName+"/exec", map[string]any{
		"Cmd": cmd, "AttachStdout": true, "AttachStderr": true,
	})
	if err != nil {
		return "", 0, err
	}
	if status != http.StatusCreated {
		return "", 0, fmt.Errorf("exec create failed (%d): %s", status, respBody)
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(respBody, &created); err != nil {
		return "", 0, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "http://docker/exec/"+created.ID+"/start", strings.NewReader(`{"Detach":false,"Tty":false}`))
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
		return "", 0, fmt.Errorf("exec start failed (%d): %s", resp.StatusCode, body)
	}
	output = demux(io.LimitReader(resp.Body, maxResponse))

	inspectBody, status, err := dockerRequest(ctx, "GET", "/exec/"+created.ID+"/json", nil)
	if err != nil {
		return output, 0, err
	}
	if status != http.StatusOK {
		return output, 0, fmt.Errorf("exec inspect failed (%d): %s", status, inspectBody)
	}
	var inspect struct {
		ExitCode int `json:"ExitCode"`
	}
	err = json.Unmarshal(inspectBody, &inspect)
	return output, inspect.ExitCode, err
}

// PostgresReady checks over TCP: the image's init-time server only listens
// on the unix socket, so this doesn't report ready before init finishes.
func PostgresReady(ctx context.Context, containerName string) bool {
	_, exitCode, err := execInContainer(ctx, containerName, []string{"pg_isready", "-h", "127.0.0.1", "-U", "postgres"})
	return err == nil && exitCode == 0
}

// PostgresHasDatabase reports whether the server has a database named name.
func PostgresHasDatabase(ctx context.Context, containerName, name string) (bool, error) {
	out, exitCode, err := execInContainer(ctx, containerName, []string{"psql", "-U", "postgres", "-Atc",
		"SELECT 1 FROM pg_database WHERE datname = '" + strings.ReplaceAll(name, "'", "''") + "'"})
	if err != nil {
		return false, err
	}
	if exitCode != 0 {
		return false, fmt.Errorf("psql exited %d: %s", exitCode, strings.TrimSpace(out))
	}
	return strings.TrimSpace(out) == "1", nil
}

// PostgresExec runs one SQL statement as the postgres superuser (local trust auth).
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

// ContainerLogs returns the last tailLines of stdout+stderr.
func ContainerLogs(ctx context.Context, containerName string, tailLines int) (string, error) {
	ctx, cancel := withDeadline(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://docker/containers/%s/logs?stdout=1&stderr=1&tail=%d&timestamps=1", containerName, tailLines), nil)
	if err != nil {
		return "", err
	}
	resp, err := dockerClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("docker daemon unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("logs failed (%d): %s", resp.StatusCode, body)
	}
	return demux(io.LimitReader(resp.Body, maxResponse)), nil
}

type containerInfo struct {
	Image  string `json:"Image"` // the image's ID, whatever tag it was run by
	Config struct {
		Env    []string          `json:"Env"`
		Cmd    []string          `json:"Cmd"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	HostConfig struct {
		Binds         []string        `json:"Binds"`
		NetworkMode   string          `json:"NetworkMode"`
		RestartPolicy json.RawMessage `json:"RestartPolicy"`
		ShmSize       int64           `json:"ShmSize"`
	} `json:"HostConfig"`
	State struct {
		Status    string `json:"Status"`
		Pid       int    `json:"Pid"`
		OOMKilled bool   `json:"OOMKilled"`
		ExitCode  int    `json:"ExitCode"`
	} `json:"State"`
	RestartCount    int `json:"RestartCount"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

func inspect(ctx context.Context, containerName string) (*containerInfo, error) {
	respBody, status, err := dockerRequest(ctx, "GET", "/containers/"+containerName+"/json", nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, nil
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("container inspect failed (%d): %s", status, respBody)
	}
	var info containerInfo
	return &info, json.Unmarshal(respBody, &info)
}

// ContainerEnv returns the container's environment, or nil if it doesn't exist.
func ContainerEnv(ctx context.Context, containerName string) (map[string]string, error) {
	info, err := inspect(ctx, containerName)
	if err != nil || info == nil {
		return nil, err
	}
	env := map[string]string{}
	for _, kv := range info.Config.Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	return env, nil
}

// StartContainer starts an existing, stopped container.
func StartContainer(ctx context.Context, containerName string) error {
	respBody, status, err := dockerRequest(ctx, "POST", "/containers/"+containerName+"/start", nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent && status != http.StatusNotModified {
		return fmt.Errorf("container start failed (%d): %s", status, respBody)
	}
	return nil
}

// ListeningPorts returns the TCP ports the container listens on, split into
// ports reachable from other containers and ones bound to loopback only.
// It reads the container's /proc/<pid>/net/tcp{,6} from the host, falling
// back to `cat` inside the container.
func ListeningPorts(ctx context.Context, containerName string) (reachable, loopback []int, err error) {
	info, err := inspect(ctx, containerName)
	if err != nil || info == nil || info.State.Pid == 0 {
		return nil, nil, err
	}
	var tables string
	for _, f := range []string{"tcp", "tcp6"} {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/net/%s", info.State.Pid, f))
		if err != nil {
			out, _, err := execInContainer(ctx, containerName, []string{"cat", "/proc/net/tcp", "/proc/net/tcp6"})
			if err != nil {
				return nil, nil, err
			}
			tables = out
			break
		}
		tables += string(b)
	}
	reachable, loopback = parseListening(tables)
	return reachable, loopback, nil
}

// dockerDNS is where Docker's embedded resolver listens in every container
// on a user-defined network, on a random port; it isn't the app's.
var dockerDNS = net.IPv4(127, 0, 0, 11)

// parseListening reads /proc/net/tcp{,6} tables. A port counts as reachable
// if any listener on it is bound beyond loopback (all of 127.0.0.0/8 and
// ::1 are loopback).
func parseListening(tables string) (reachable, loopback []int) {
	onLoopback, beyond := map[int]bool{}, map[int]bool{}
	for _, line := range strings.Split(tables, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[3] != "0A" { // 0A = LISTEN
			continue
		}
		addrHex, portHex, ok := strings.Cut(fields[1], ":")
		if !ok {
			continue
		}
		port, err := strconv.ParseInt(portHex, 16, 32)
		ip := procAddr(addrHex)
		if err != nil || ip == nil || ip.Equal(dockerDNS) {
			continue
		}
		if ip.IsLoopback() {
			onLoopback[int(port)] = true
		} else {
			beyond[int(port)] = true
		}
	}
	for p := range beyond {
		reachable = append(reachable, p)
	}
	for p := range onLoopback {
		if !beyond[p] {
			loopback = append(loopback, p)
		}
	}
	sort.Ints(reachable)
	sort.Ints(loopback)
	return reachable, loopback
}

// procAddr decodes an address from /proc/net/tcp{,6}: hex of 32-bit words,
// each in the host's (little-endian) byte order.
func procAddr(h string) net.IP {
	b, err := hex.DecodeString(h)
	if err != nil || (len(b) != net.IPv4len && len(b) != net.IPv6len) {
		return nil
	}
	for i := 0; i < len(b); i += 4 {
		b[i], b[i+1], b[i+2], b[i+3] = b[i+3], b[i+2], b[i+1], b[i]
	}
	return net.IP(b)
}

// ExposedPort returns the first port an image EXPOSEs, 0 if none.
func ExposedPort(ctx context.Context, image string) int {
	respBody, status, err := dockerRequest(ctx, "GET", "/images/"+url.PathEscape(image)+"/json", nil)
	if err != nil || status != http.StatusOK {
		return 0
	}
	var img struct {
		Config struct {
			ExposedPorts map[string]struct{} `json:"ExposedPorts"`
		} `json:"Config"`
	}
	if json.Unmarshal(respBody, &img) != nil {
		return 0
	}
	ports := []int{}
	for p := range img.Config.ExposedPorts {
		if n, err := strconv.Atoi(strings.TrimSuffix(p, "/tcp")); err == nil {
			ports = append(ports, n)
		}
	}
	sort.Ints(ports)
	if len(ports) == 0 {
		return 0
	}
	return ports[0]
}

// ContainerIP returns the container's address on network.
func ContainerIP(ctx context.Context, containerName, network string) (string, error) {
	info, err := inspect(ctx, containerName)
	if err != nil {
		return "", err
	}
	if info == nil {
		return "", fmt.Errorf("container %q not found", containerName)
	}
	n, ok := info.NetworkSettings.Networks[network]
	if !ok || n.IPAddress == "" {
		return "", fmt.Errorf("container %q has no IP on network %q", containerName, network)
	}
	return n.IPAddress, nil
}

// State is a container's state, as deploys and health checks read it.
type State struct {
	Status    string // docker's state ("running", "exited", ...), "not found" or "unknown"
	Restarts  int    // surfaces crash loops
	OOMKilled bool   // the last exit was the kernel killing it for memory
	ExitCode  int    // of the last exit; 137 is SIGKILL
}

func ContainerState(ctx context.Context, containerName string) State {
	info, err := inspect(ctx, containerName)
	if err != nil {
		return State{Status: "unknown"}
	}
	if info == nil {
		return State{Status: "not found"}
	}
	return State{Status: info.State.Status, Restarts: info.RestartCount, OOMKilled: info.State.OOMKilled, ExitCode: info.State.ExitCode}
}

// ContainerStatus is ContainerState's status and restart count.
func ContainerStatus(ctx context.Context, containerName string) (status string, restarts int) {
	st := ContainerState(ctx, containerName)
	return st.Status, st.Restarts
}

// Death is why a container stopped by itself, Docker sent it no signal.
type Death struct {
	OOM bool // the kernel killed it for running out of memory
	// Sure, for OOM: Docker's "oom" event said so. Under rootless Docker a
	// main process the kernel kills often exits with no "oom" event, so a
	// SIGKILL death (exit 137) Docker didn't send counts too: the kernel's
	// OOM killer is almost always what sends it.
	Sure     bool
	ExitCode string // otherwise: it exited with this code, never "0"
}

// oomEventGrace is how long a SIGKILL death waits for a late "oom" event.
const oomEventGrace = time.Second

// TagImage points targetRef ("repo:tag") at sourceRef's image.
func TagImage(ctx context.Context, sourceRef, targetRef string) error {
	repo, tag := targetRef, "latest"
	if i := strings.LastIndex(targetRef, ":"); i != -1 {
		repo, tag = targetRef[:i], targetRef[i+1:]
	}
	respBody, status, err := dockerRequest(ctx, "POST", "/images/"+url.PathEscape(sourceRef)+"/tag?repo="+url.QueryEscape(repo)+"&tag="+url.QueryEscape(tag), nil)
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return fmt.Errorf("image tag failed (%d): %s", status, respBody)
	}
	return nil
}

func ImageExists(ctx context.Context, ref string) (bool, error) {
	_, status, err := dockerRequest(ctx, "GET", "/images/"+url.PathEscape(ref)+"/json", nil)
	if err != nil {
		return false, err
	}
	switch status {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("image inspect failed (%d)", status)
	}
}

// ImageID returns the ID ref points at, "" if there's no such image.
func ImageID(ctx context.Context, ref string) string {
	respBody, status, err := dockerRequest(ctx, "GET", "/images/"+url.PathEscape(ref)+"/json", nil)
	if err != nil || status != http.StatusOK {
		return ""
	}
	var img struct {
		ID string `json:"Id"`
	}
	json.Unmarshal(respBody, &img)
	return img.ID
}

// RemoveImage removes a tag, or an image by ID, and the image itself once
// nothing else references it. Missing images and images still used by a
// container are left alone without an error.
func RemoveImage(ctx context.Context, ref string) error {
	respBody, status, err := dockerRequest(ctx, "DELETE", "/images/"+url.PathEscape(ref), nil)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK, http.StatusNotFound, http.StatusConflict:
		return nil
	}
	return fmt.Errorf("image remove %s failed (%d): %s", ref, status, respBody)
}

// DockerRootDir is where Docker keeps images, containers and volumes.
func DockerRootDir(ctx context.Context) (string, error) {
	respBody, status, err := dockerRequest(ctx, "GET", "/info", nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("docker info failed (%d)", status)
	}
	var info struct {
		DockerRootDir string `json:"DockerRootDir"`
	}
	if err := json.Unmarshal(respBody, &info); err != nil {
		return "", err
	}
	return info.DockerRootDir, nil
}

// Disk reports usage of the filesystem Docker stores images and volumes on.
func Disk(ctx context.Context) (used, total uint64, err error) {
	root, err := DockerRootDir(ctx)
	if err != nil {
		return 0, 0, err
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(root, &st); err != nil {
		return 0, 0, err
	}
	total = st.Blocks * uint64(st.Bsize)
	return total - st.Bavail*uint64(st.Bsize), total, nil
}

// Listener is a TCP socket listening, from /proc/net/tcp{,6}.
type Listener struct {
	IP   net.IP
	Port int
}

// Listeners reads every listening socket from /proc/net/tcp{,6} tables.
func Listeners(tables string) []Listener {
	var out []Listener
	for _, line := range strings.Split(tables, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[3] != "0A" { // 0A = LISTEN
			continue
		}
		addrHex, portHex, ok := strings.Cut(fields[1], ":")
		if !ok {
			continue
		}
		port, err := strconv.ParseInt(portHex, 16, 32)
		ip := procAddr(addrHex)
		if err != nil || ip == nil {
			continue
		}
		out = append(out, Listener{IP: ip, Port: int(port)})
	}
	return out
}

// RunningContainer is a container Docker runs, by name without the slash.
type RunningContainer struct {
	ID   string
	Name string
	App  string // its AppLabel, "" if none
}

// RunningContainers lists the running containers.
func RunningContainers(ctx context.Context) ([]RunningContainer, error) {
	respBody, status, err := dockerRequest(ctx, "GET", "/containers/json", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("container list failed (%d): %s", status, respBody)
	}
	var list []struct {
		ID     string            `json:"Id"`
		Names  []string          `json:"Names"`
		Labels map[string]string `json:"Labels"`
	}
	if err := json.Unmarshal(respBody, &list); err != nil {
		return nil, err
	}
	out := make([]RunningContainer, 0, len(list))
	for _, c := range list {
		if len(c.Names) == 0 {
			continue
		}
		out = append(out, RunningContainer{ID: c.ID, Name: strings.TrimPrefix(c.Names[0], "/"), App: c.Labels[AppLabel]})
	}
	return out, nil
}

// PostgresQueryIn runs sql with psql in database db and returns its rows.
func PostgresQueryIn(ctx context.Context, containerName, db, sql string) ([][]string, error) {
	out, exitCode, err := execInContainer(ctx, containerName, []string{"psql", "-U", "postgres", "-d", db, "-Atc", sql})
	if err != nil {
		return nil, err
	}
	if exitCode != 0 {
		return nil, fmt.Errorf("psql exited %d: %s", exitCode, strings.TrimSpace(out))
	}
	var rows [][]string
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if line != "" {
			rows = append(rows, strings.Split(line, "|"))
		}
	}
	return rows, nil
}
