package build

import (
	"fmt"
	"io"
	"os"
	"os/exec"
)

func BuildWithStrategy(sourceDir, imageTag, strategy string, out io.Writer) error {
	if strategy == "dockerfile" {
		return buildWithDocker(sourceDir, imageTag, out)
	}
	// "nixpacks" kept as legacy alias for existing projects.
	return buildWithRailpack(sourceDir, imageTag, out)
}

func buildWithDocker(sourceDir, imageTag string, out io.Writer) error {
	cmd := exec.Command("docker", "build", "-t", imageTag, sourceDir)
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd.Run()
}

func buildWithRailpack(sourceDir, imageTag string, out io.Writer) error {
	ensureBuildKit(out)
	cmd := exec.Command("railpack", "build", sourceDir, "--name", imageTag)
	cmd.Stdout = out
	cmd.Stderr = out
	// Pass through BUILDKIT_HOST if set; railpack needs it.
	if h := os.Getenv("BUILDKIT_HOST"); h != "" {
		cmd.Env = append(os.Environ(), "BUILDKIT_HOST="+h)
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("railpack build failed (need railpack + buildkit? see https://railpack.com): %w", err)
	}
	return nil
}

// ensureBuildKit starts a local buildkitd container if BUILDKIT_HOST is unset
// and docker is available. Best-effort: failures are ignored, railpack will
// report a clear error itself.
func ensureBuildKit(out io.Writer) {
	if os.Getenv("BUILDKIT_HOST") != "" {
		return
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return
	}
	// Already running?
	if err := exec.Command("docker", "inspect", "buildkit").Run(); err == nil {
		os.Setenv("BUILDKIT_HOST", "docker-container://buildkit")
		return
	}
	fmt.Fprintln(out, "starting buildkit container for railpack...")
	if err := exec.Command("docker", "run", "--rm", "--privileged", "-d", "--name", "buildkit", "moby/buildkit").Run(); err != nil {
		return
	}
	os.Setenv("BUILDKIT_HOST", "docker-container://buildkit")
}
