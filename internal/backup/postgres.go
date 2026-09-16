package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"os/exec"
)

// DumpDatabase runs pg_dump inside the database container and returns the
// gzip-compressed result. This shells out to the docker CLI (like
// internal/build's railpack build does) rather than using hako's
// usual raw Docker HTTP API client, because pg_dump's stdout is the actual
// dump content and must not be mixed with its stderr progress output —
// os/exec keeps the two streams separate for free, where the Docker API's
// exec/start endpoint would need its multiplexed stream frames demuxed by
// hand to get the same guarantee.
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

// RestoreDatabase pipes a gzip-compressed dump (as produced by
// DumpDatabase) into psql inside the database container. pg_dump's default
// plain-SQL output uses INSERT/COPY statements, not DROP+CREATE, so
// restoring into an already-populated database can fail loudly on
// conflicting rows rather than silently overwrite them — safe by accident,
// but callers restoring into a live database should know it's not a clean
// slate.
func RestoreDatabase(ctx context.Context, containerName, dbUser, dbName string, gzipDump []byte) error {
	r, err := gzip.NewReader(bytes.NewReader(gzipDump))
	if err != nil {
		return fmt.Errorf("invalid backup archive: %w", err)
	}
	defer r.Close()

	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", containerName, "psql", "-U", dbUser, "-d", dbName)
	cmd.Stdin = r
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("restore failed: %w: %s", err, stderr.String())
	}
	return nil
}
