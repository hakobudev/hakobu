package node

import (
	"archive/tar"
	"context"
	"fmt"
	"io"

	"github.com/hakobudev/hakobu/internal/backup"
	"github.com/hakobudev/hakobu/internal/config"
	"github.com/hakobudev/hakobu/internal/deploy"
	"github.com/hakobudev/hakobu/internal/panellog"
)

// The same backups as BackupDatabase and BackupVolume, as plain streams:
// sealing and storing them is the caller's.

// DumpDatabase writes the database's dump (pg_dump's custom format) to w.
func (Local) DumpDatabase(ctx context.Context, d DBSpec, w io.Writer) error {
	return backup.DumpDatabase(ctx, PostgresContainer, d.User, d.Name, w)
}

// CheckDump restores dump into a scratch database next to d, counts its
// tables and drops it.
func (Local) CheckDump(ctx context.Context, d DBSpec, dump io.Reader) (tables int, err error) {
	// Database names can't contain dots, so this never clashes with one.
	scratch := "hakobu.verify." + d.Name
	drop, err := scratchDatabase(ctx, d, scratch)
	if err != nil {
		return 0, err
	}
	defer drop()
	if err := backup.RestoreDatabase(ctx, PostgresContainer, d.User, scratch, dump); err != nil {
		return 0, err
	}
	return backup.CountTables(ctx, PostgresContainer, d.User, scratch)
}

// LoadDump replaces the database's content with dump (replaceWith).
func (Local) LoadDump(ctx context.Context, d DBSpec, dump io.Reader) error {
	return replaceWith(ctx, d, dump)
}

// ArchiveVolume writes a tar of the volume to w, with the running
// containers that mount it paused meanwhile. One that can't be paused is
// copied running: a backup that may need the app's own recovery beats none.
func (Local) ArchiveVolume(ctx context.Context, app, name string, w io.Writer) error {
	volume := Volume(app, name)
	users, err := deploy.RunningWithVolume(ctx, volume)
	if err != nil {
		panellog.Warn("backup of", volume+": copying without pausing its containers:", err)
	}
	for _, c := range users {
		if err := deploy.PauseContainer(ctx, c); err != nil {
			panellog.Warn("backup of", volume+": copying while", c, "runs:", err)
			continue
		}
		defer func() {
			if err := deploy.UnpauseContainer(ctx, c); err != nil {
				panellog.Error("failed to unpause", c+":", err)
			}
		}()
	}
	return backup.ArchiveVolume(ctx, config.PostgresImage, volume, w)
}

// CheckArchive reads a whole volume tar, counting its files.
func (Local) CheckArchive(archive io.Reader) (files int, err error) {
	tr := tar.NewReader(archive)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return files, nil
		}
		if err != nil {
			return files, fmt.Errorf("the tar is broken: %w", err)
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return files, err
		}
		if h.Typeflag != tar.TypeDir {
			files++
		}
	}
}

// LoadArchive replaces the volume's contents with archive, the app and its
// worker stopped meanwhile and started again after.
func (n Local) LoadArchive(ctx context.Context, app AppSpec, name string, archive io.Reader, out io.Writer) error {
	fmt.Fprintln(out, "stopping", app.Name)
	if err := n.StopApp(ctx, app); err != nil {
		return err
	}
	defer n.StartApp(ctx, app, out)
	fmt.Fprintln(out, "replacing the contents of volume", name)
	return backup.RestoreVolume(ctx, config.PostgresImage, Volume(app.Name, name), archive)
}
