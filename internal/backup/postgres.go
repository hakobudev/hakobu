package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"os/exec"
)

// DumpDatabase returns a gzipped pg_dump. It uses the docker CLI so stdout
// (the dump) and stderr stay separate without demuxing the exec stream.
func DumpDatabase(ctx context.Context, containerName, dbUser, dbName string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", "exec", containerName, "pg_dump", "-U", dbUser, "-d", dbName)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("pg_dump failed: %w: %s", err, stderr.String())
	}
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	if _, err := w.Write(stdout.Bytes()); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return gz.Bytes(), nil
}

func RestoreDatabase(ctx context.Context, containerName, dbUser, dbName string, gzipDump []byte) error {
	r, err := gzip.NewReader(bytes.NewReader(gzipDump))
	if err != nil {
		return fmt.Errorf("invalid backup archive: %w", err)
	}
	defer r.Close()
	// One transaction that stops at the first error: a restore either fully
	// applies or changes nothing.
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", containerName, "psql", "-U", dbUser, "-d", dbName, "-v", "ON_ERROR_STOP=1", "--single-transaction")
	cmd.Stdin = r
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("restore failed: %w: %s", err, stderr.String())
	}
	return nil
}
