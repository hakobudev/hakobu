package build

import (
	"io"
	"os"
	"os/exec"
)

// CloneRepo clones cloneURL into destDir without exposing the token in the
// process list: the token travels via a GIT_ASKPASS helper + env var
// instead of being embedded in the URL (visible in `ps aux`).
func CloneRepo(cloneURL, token, destDir string, out io.Writer) error {
	if err := os.RemoveAll(destDir); err != nil {
		return err
	}

	askPass, err := gitAskPassHelper()
	if err != nil {
		return err
	}

	cmd := exec.Command("git", "clone", "--depth=1", cloneURL, destDir)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Env = append(os.Environ(),
		"GIT_ASKPASS="+askPass,
		"GIT_HAKO_TOKEN="+token,
		"GIT_TERMINAL_PROMPT=0",
	)
	return cmd.Run()
}

// gitAskPassHelper writes a tiny executable that prints the token from the
// environment. Cached per-process via os.CreateTemp cleanup on exit is
// skipped intentionally — file lives in /tmp and is reused.
func gitAskPassHelper() (string, error) {
	f, err := os.CreateTemp("", "hako-askpass-*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if _, err := f.WriteString("#!/bin/sh\nexec echo \"$GIT_HAKO_TOKEN\"\n"); err != nil {
		f.Close()
		return "", err
	}
	f.Close()
	if err := os.Chmod(name, 0700); err != nil {
		return "", err
	}
	return name, nil
}
