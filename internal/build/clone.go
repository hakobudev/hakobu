package build

import (
	"io"
	"os"
	"os/exec"
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

	cmd := exec.Command("git", "clone", "--depth=1", cloneURL, destDir)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Env = append(os.Environ(),
		"GIT_ASKPASS="+askPass.Name(),
		"GIT_HAKOBU_TOKEN="+token,
		"GIT_TERMINAL_PROMPT=0",
	)
	return cmd.Run()
}
