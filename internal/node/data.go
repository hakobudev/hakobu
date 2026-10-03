package node

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/x0ryz/hakobu/internal/backup"
	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/deploy"
	"github.com/x0ryz/hakobu/internal/secret"
)

// PostgresContainer is the node's one shared Postgres service; every
// database is a logical database and role inside it, reached by this name
// on its project's network. Tests run their own.
var PostgresContainer = "hakobu-postgres"

// DBSpec is a database as the node needs it.
type DBSpec struct {
	Name, User string
	Password   string // hakobu's own hex, never quoted text
}

// EnsurePostgres starts the shared service on first use; an existing
// container is reused, never recreated. The superuser password only
// satisfies the image's startup check: hakobu administers the service over
// `docker exec` with local trust auth.
func (Local) EnsurePostgres(ctx context.Context) error {
	existing, err := deploy.ContainerEnv(ctx, PostgresContainer)
	if err != nil {
		return err
	}
	if existing != nil {
		err = deploy.StartContainer(ctx, PostgresContainer)
	} else {
		b := make([]byte, 16)
		if _, err = rand.Read(b); err != nil {
			return err
		}
		// Postgres 18+ images expect the volume at /var/lib/postgresql, older ones work with it too.
		_, err = deploy.RunServiceContainer(ctx, PostgresContainer, config.PostgresImage, []string{"POSTGRES_PASSWORD=" + hex.EncodeToString(b)}, "/var/lib/postgresql")
	}
	if err != nil {
		return err
	}
	if !deploy.WaitHealthy(60, time.Second, func() bool { return deploy.PostgresReady(ctx, PostgresContainer) }) {
		return fmt.Errorf("postgres service failed to become ready")
	}
	return nil
}

func (Local) PostgresReady(ctx context.Context) bool {
	return deploy.PostgresReady(ctx, PostgresContainer)
}

// CreateDatabase creates the database and its role, which alone may
// connect to it.
func (Local) CreateDatabase(ctx context.Context, d DBSpec) error {
	for _, sql := range []string{
		fmt.Sprintf(`CREATE USER "%s" WITH PASSWORD '%s'`, d.User, d.Password),
		fmt.Sprintf(`CREATE DATABASE "%s" OWNER "%s"`, d.Name, d.User),
		// New databases are connectable by PUBLIC; keep other projects' roles out.
		fmt.Sprintf(`REVOKE CONNECT ON DATABASE "%s" FROM PUBLIC`, d.Name),
	} {
		if err := deploy.PostgresExec(ctx, PostgresContainer, sql); err != nil {
			return err
		}
	}
	return nil
}

// EnsureDatabase creates the database, empty, with its role if Postgres
// lacks it, as on a new server the panel was restored to.
func (n Local) EnsureDatabase(ctx context.Context, d DBSpec) error {
	exists, err := deploy.PostgresHasDatabase(ctx, PostgresContainer, d.Name)
	if err != nil || exists {
		return err
	}
	return n.CreateDatabase(ctx, d)
}

func (Local) DropDatabase(ctx context.Context, d DBSpec) error {
	for _, sql := range []string{
		fmt.Sprintf(`DROP DATABASE IF EXISTS "%s" WITH (FORCE)`, d.Name),
		fmt.Sprintf(`DROP USER IF EXISTS "%s"`, d.User),
	} {
		if err := deploy.PostgresExec(ctx, PostgresContainer, sql); err != nil {
			return err
		}
	}
	return nil
}

func (Local) SetPassword(ctx context.Context, d DBSpec) error {
	return deploy.PostgresExec(ctx, PostgresContainer, fmt.Sprintf(`ALTER USER "%s" WITH PASSWORD '%s'`, d.User, d.Password))
}

// scratchDatabase (re)creates an empty database owned by d's role and
// returns what drops it again.
func scratchDatabase(ctx context.Context, d DBSpec, scratch string) (drop func(), err error) {
	dropSQL := fmt.Sprintf(`DROP DATABASE IF EXISTS "%s" WITH (FORCE)`, scratch)
	for _, sql := range []string{
		dropSQL, // left over from an interrupted job
		fmt.Sprintf(`CREATE DATABASE "%s" OWNER "%s"`, scratch, d.User),
		fmt.Sprintf(`REVOKE CONNECT ON DATABASE "%s" FROM PUBLIC`, scratch),
	} {
		if err := deploy.PostgresExec(ctx, PostgresContainer, sql); err != nil {
			return nil, err
		}
	}
	return func() {
		if err := deploy.PostgresExec(ctx, PostgresContainer, dropSQL); err != nil {
			fmt.Println("failed to drop", scratch+":", err)
		}
	}, nil
}

// Snapshots: before each deploy the app's database is dumped on its node,
// next to the Previous image, so Rollback can return the data a bad
// migration broke together with the code. It's an undo, not a backup: it
// lives on the node and only the last one is kept. Dumps are sealed with
// the master key (secret.NewFileWriter): a copy of data/ without the key
// doesn't give away the apps' data.

const (
	snapshotDir = "data/snapshots"
	snapshotExt = ".dump.enc"
)

// Dump is a sealed dump on the node: one just taken, or an app's snapshot.
// The panel only passes it back.
type Dump string

// SnapshotOf is the app's snapshot.
func SnapshotOf(app string) Dump { return Dump(app + snapshotExt) }

func (d Dump) path() string { return filepath.Join(snapshotDir, filepath.Base(string(d))) }

// SaveDump writes a sealed dump of the database.
func (Local) SaveDump(ctx context.Context, d DBSpec) (dump Dump, err error) {
	if err := os.MkdirAll(snapshotDir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(snapshotDir, "dump-*.tmp")
	if err != nil {
		return "", err
	}
	defer f.Close()
	defer func() {
		if err != nil {
			os.Remove(f.Name())
		}
	}()
	w, err := secret.NewFileWriter(f)
	if err != nil {
		return "", err
	}
	if err := backup.DumpDatabase(ctx, PostgresContainer, d.User, d.Name, w); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	return Dump(filepath.Base(f.Name())), f.Sync()
}

// DropDump deletes a dump; one that isn't there (kept as a snapshot
// meanwhile, say) is no error.
func (Local) DropDump(dump Dump) {
	if dump != "" {
		os.Remove(dump.path())
	}
}

// KeepSnapshot makes the dump the app's snapshot, or with dump "" drops it.
func (Local) KeepSnapshot(app string, dump Dump) error {
	if dump == "" {
		os.Remove(SnapshotOf(app).path())
		return nil
	}
	return os.Rename(dump.path(), SnapshotOf(app).path())
}

// HasSnapshot reports whether the app's snapshot file is there.
func (Local) HasSnapshot(app string) bool {
	_, err := os.Stat(SnapshotOf(app).path())
	return err == nil
}

// PruneSnapshots deletes the snapshots of apps not in apps, and dumps
// left by interrupted jobs; a data copy saved by a failed rollback stays
// until someone deals with it.
func (Local) PruneSnapshots(apps map[string]bool) {
	snaps, err := os.ReadDir(snapshotDir)
	if err != nil {
		return
	}
	for _, f := range snaps {
		name, isSnapshot := strings.CutSuffix(f.Name(), snapshotExt)
		info, err := f.Info()
		stale := err == nil && time.Since(info.ModTime()) > 24*time.Hour
		if (isSnapshot && !apps[name] && !strings.HasSuffix(name, "-before-rollback")) || (strings.HasSuffix(f.Name(), ".tmp") && stale) {
			os.Remove(filepath.Join(snapshotDir, f.Name()))
		}
	}
}

// SetAside keeps a dump that couldn't be put back under a name of the
// app's and says where it is.
func (Local) SetAside(app string, dump Dump) string {
	keep := filepath.Join(snapshotDir, app+"-before-rollback"+snapshotExt)
	if err := os.Rename(dump.path(), keep); err != nil {
		return dump.path()
	}
	return keep
}

// ReplaceDatabase swaps the database's content for a dump: the dump is
// restored into a scratch database first, so a broken one changes
// nothing, then renamed over it. Nothing may be connected to it.
func (Local) ReplaceDatabase(ctx context.Context, d DBSpec, dump Dump) error {
	f, err := os.Open(dump.path())
	if err != nil {
		return err
	}
	defer f.Close()
	r, err := secret.NewFileReader(f)
	if err != nil {
		return fmt.Errorf("snapshot %s: %w", dump, err)
	}
	// Database names can't contain dots, so this never clashes with one.
	scratch := "hakobu.restore." + d.Name
	drop, err := scratchDatabase(ctx, d, scratch)
	if err != nil {
		return err
	}
	if err := backup.RestoreDatabase(ctx, PostgresContainer, d.User, scratch, r); err != nil {
		drop()
		return err
	}
	for _, sql := range []string{
		fmt.Sprintf(`DROP DATABASE "%s" WITH (FORCE)`, d.Name),
		fmt.Sprintf(`ALTER DATABASE "%s" RENAME TO "%s"`, scratch, d.Name),
	} {
		if err := deploy.PostgresExec(ctx, PostgresContainer, sql); err != nil {
			return err
		}
	}
	return nil
}

// Volumes. volumePrefix marks the Docker volumes hakobu creates for apps.
// App and volume names can't contain "_", so "<app>_<volume>" is
// unambiguous.
const volumePrefix = "hakobu-vol-"

// Volume is the Docker volume of an app's volume.
func Volume(app, name string) string { return volumePrefix + app + "_" + name }

func (Local) HasVolume(ctx context.Context, app, name string) (bool, error) {
	return deploy.VolumeExists(ctx, Volume(app, name))
}

func (Local) RemoveVolume(ctx context.Context, app, name string) error {
	return deploy.RemoveVolume(ctx, Volume(app, name))
}

// RemoveVolumesExcept deletes the app volumes not in keep (by Volume
// name); volumes still attached to a container are skipped.
func (Local) RemoveVolumesExcept(ctx context.Context, keep map[string]bool) error {
	names, err := deploy.VolumeNames(ctx, volumePrefix)
	if err != nil {
		return err
	}
	for _, n := range names {
		if !keep[n] {
			if err := deploy.RemoveVolume(ctx, n); err != nil {
				return err
			}
		}
	}
	return nil
}

// RemoveApp deletes the app's containers, builds, clone, snapshot and the
// given volumes with their data.
func (n Local) RemoveApp(ctx context.Context, app string, volumes []string) error {
	n.RemoveProxy(app)
	for _, c := range []string{app + "-blue", app + "-green", WorkerContainer(app)} {
		if err := deploy.RemoveContainer(ctx, c); err != nil {
			return err
		}
	}
	for _, img := range []Image{Latest, Previous, Next} {
		n.DropImage(ctx, app, img)
	}
	os.RemoveAll(WorkDir(app))
	os.Remove(SnapshotOf(app).path())
	for _, v := range volumes {
		if err := n.RemoveVolume(ctx, app, v); err != nil {
			return err
		}
	}
	return nil
}
