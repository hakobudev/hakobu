package deploy

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// RegistryAuth signs into the registry an image is pulled from; the zero
// value pulls anonymously.
type RegistryAuth struct {
	Username, Password string
}

// PullImage pulls ref, a "repo@sha256:…" pinned by digest. Docker answers a
// pull with 200 before it starts, then streams progress, so a refused or
// missing image shows only as an error in the stream.
func PullImage(ctx context.Context, ref string, auth RegistryAuth) error {
	ctx, cancel := withDeadline(ctx, requestTimeout("/images/create"))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", "http://docker/images/create?fromImage="+url.QueryEscape(ref), nil)
	if err != nil {
		return err
	}
	if auth.Username != "" {
		b, _ := json.Marshal(map[string]string{"username": auth.Username, "password": auth.Password})
		req.Header.Set("X-Registry-Auth", base64.URLEncoding.EncodeToString(b))
	}
	resp, err := dockerClient.Do(req)
	if err != nil {
		return fmt.Errorf("docker daemon unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("pulling %s failed (%d): %s", ref, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return pullError(ref, resp.Body)
}

// pullError reads a pull's progress stream for the error that ends it.
func pullError(ref string, stream io.Reader) error {
	sc := bufio.NewScanner(stream)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var msg struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) == nil && msg.Error != "" {
			return fmt.Errorf("pulling %s failed: %s", ref, msg.Error)
		}
	}
	return sc.Err()
}

// ErrReleaseFailed: the release command ran and exited with an error.
var ErrReleaseFailed = errors.New("release command failed")

// RunOnce runs command in a container of imageTag to its end, as an app's
// release step (migrations) before the new version starts, and removes the
// container. It returns the command's last output either way, and
// ErrReleaseFailed wrapped with the exit code if it didn't exit 0.
func RunOnce(ctx context.Context, imageTag, name, command string, opts AppOptions, timeout time.Duration) (output string, err error) {
	opts.Command = command
	spec, hostConfig := opts.spec(imageTag)
	hostConfig["RestartPolicy"] = map[string]string{"Name": "no"}
	id, err := runContainer(ctx, name, spec, hostConfig)
	if err != nil {
		return "", err
	}
	parent := ctx
	keep := false
	defer func() {
		if !keep {
			_ = RemoveContainer(context.Background(), name)
		}
	}()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	respBody, status, err := dockerRequest(ctx, "POST", "/containers/"+id+"/wait", nil)
	output, _ = ContainerLogs(context.Background(), name, 100)
	if parent.Err() != nil {
		// hakobu is stopping: the command goes on to its end rather than
		// be cut off mid-migration, and hakobu removes it as it starts.
		keep = true
		return output, fmt.Errorf("%w: hakobu stopped while it ran; it goes on to its end, and the deploy is tried again as hakobu starts", ErrReleaseFailed)
	}
	if ctx.Err() != nil {
		return output, fmt.Errorf("%w: still running after %s", ErrReleaseFailed, timeout)
	}
	if err != nil {
		return output, err
	}
	if status != http.StatusOK {
		return output, fmt.Errorf("container wait failed (%d): %s", status, respBody)
	}
	var waited struct {
		StatusCode int `json:"StatusCode"`
	}
	if err := json.Unmarshal(respBody, &waited); err != nil {
		return output, err
	}
	if waited.StatusCode != 0 {
		return output, fmt.Errorf("%w: exit %d", ErrReleaseFailed, waited.StatusCode)
	}
	return output, nil
}
