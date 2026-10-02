package build

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/x0ryz/hakobu/internal/config"
)

func BuildWithStrategy(sourceDir, imageTag, strategy string, out io.Writer) error {
	if strategy == "dockerfile" {
		return run(out, "docker", "build", "-t", imageTag, sourceDir)
	}
	ensureBuildKit(out)
	if err := run(out, "railpack", "build", sourceDir, "--name", imageTag); err != nil {
		return fmt.Errorf("railpack build failed (need railpack + buildkit, see https://railpack.com): %w", err)
	}
	return nil
}

// buildTimeout ends a build that hangs, which would otherwise hold the
// app's deploys until hakobu restarts.
const buildTimeout = time.Hour

func run(out io.Writer, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
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
