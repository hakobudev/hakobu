package build

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

// CloneRepo shallow-clones cloneURL into destDir. The token goes through a
// GIT_ASKPASS helper so it never shows up in the process list.
func CloneRepo(cloneURL, token, destDir string, out io.Writer) error {
	if err := os.RemoveAll(destDir); err != nil {
		return err
	}
	askPass, err := os.CreateTemp("", "hakobu-askpass-*")
	if err != nil {
		return err
	}
	defer os.Remove(askPass.Name())
	_, err = askPass.WriteString("#!/bin/sh\nexec echo \"$GIT_HAKOBU_TOKEN\"\n")
	askPass.Close()
	if err != nil {
		return err
	}
	if err := os.Chmod(askPass.Name(), 0o700); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), cloneTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth=1", "--", cloneURL, destDir)
	cmd.WaitDelay = 10 * time.Second
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Env = append(os.Environ(),
		"GIT_ASKPASS="+askPass.Name(),
		"GIT_HAKOBU_TOKEN="+token,
		"GIT_TERMINAL_PROMPT=0",
	)
	err = cmd.Run()
	if ctx.Err() != nil {
		return fmt.Errorf("gave up after %s", cloneTimeout)
	}
	return err
}

// cloneTimeout ends a clone that hangs on the network.
const cloneTimeout = 10 * time.Minute
