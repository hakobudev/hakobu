package build

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/x0ryz/hakobu/internal/config"
)

// BuildWithStrategy builds sourceDir into imageTag. env ("NAME=value")
// is what the build gets: Railpack reads RAILPACK_* as settings, and both
// Railpack and a Dockerfile's ARGs take the rest. The values go in the
// build's environment, only their names on its command line, which anyone
// on the server can read.
func BuildWithStrategy(sourceDir, imageTag, strategy string, env []string, out io.Writer) error {
	var names []string
	seen := map[string]bool{}
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	if strategy == "dockerfile" {
		args := []string{"build", "-t", imageTag}
		for _, name := range names {
			args = append(args, "--build-arg", name)
		}
		return run(out, env, "docker", append(args, sourceDir)...)
	}
	ensureBuildKit(out)
	args := []string{"build", sourceDir, "--name", imageTag}
	for _, name := range names {
		args = append(args, "--env", name)
	}
	if err := run(out, env, "railpack", args...); err != nil {
		return fmt.Errorf("railpack build failed (need railpack + buildkit, see https://railpack.com): %w", err)
	}
	return nil
}

// buildTimeout ends a build that hangs, which would otherwise hold the
// app's deploys until hakobu restarts.
const buildTimeout = time.Hour

// run runs name with env added to hakobu's own environment.
func run(out io.Writer, env []string, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...) // the last of a name wins
	}
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.WaitDelay = 10 * time.Second
	err := cmd.Run()
	if ctx.Err() != nil {
		return fmt.Errorf("gave up after %s", buildTimeout)
	}
	return err
}

// ensureBuildKit points railpack at the "buildkit" container install.sh
// starts, starting it if missing, unless BUILDKIT_HOST is already set.
func ensureBuildKit(out io.Writer) {
	if os.Getenv("BUILDKIT_HOST") != "" {
		return
	}
	if exec.Command("docker", "inspect", "buildkit").Run() != nil {
		fmt.Fprintln(out, "starting buildkit container for railpack...")
		if b, err := exec.Command("docker", "run", "--privileged", "-d", "--restart", "unless-stopped", "--name", "buildkit", config.BuildKitImage).CombinedOutput(); err != nil {
			fmt.Fprintf(out, "failed to start buildkit: %v: %s\n", err, b)
			return
		}
	}
	os.Setenv("BUILDKIT_HOST", "docker-container://buildkit")
}
