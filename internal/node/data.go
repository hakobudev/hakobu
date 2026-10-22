package node

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/hakobudev/hakobu/internal/backup"
	"github.com/hakobudev/hakobu/internal/config"
	"github.com/hakobudev/hakobu/internal/deploy"
	"github.com/hakobudev/hakobu/internal/panellog"
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
		shm, _ := postgresShm() // unknown: Docker's 64 MB, and FitPostgres tries again
		_, err = deploy.RunServiceContainer(ctx, PostgresContainer, config.PostgresImage, []string{"POSTGRES_PASSWORD=" + hex.EncodeToString(b)}, "/var/lib/postgresql", shm)
	}
	if err != nil {
		return err
	}
	if !deploy.WaitHealthy(60, time.Second, func() bool { return deploy.PostgresReady(ctx, PostgresContainer) }) {
		return fmt.Errorf("postgres service failed to become ready")
	}
	return nil
}

// postgresShm is the /dev/shm Postgres gets: it keeps the memory of
// parallel queries, hashes and sorts there, and Docker's 64 MB fail them
// with "could not resize shared memory segment". A quarter of the RAM at
// most, so one big query hits a clean error before the host runs out, and
// in steps of 64 MB, so the size stays put as the kernel's share moves.
func postgresShm() (int64, error) {
	total, _, err := readMemInfo()
	if err != nil {
		return 0, err
	}
	const step = 64 << 20
	return max(step, min(1<<30, total/4)/step*step), nil
}

// FitPostgres makes the Postgres container anew if its /dev/shm isn't
// postgresShm, keeping its data, after a clean shutdown. Apps lose their
// connections for the seconds it takes; the caller connects it to the
// projects' networks again. A node without Postgres is left alone.
func (Local) FitPostgres(ctx context.Context) error {
	want, err := postgresShm()
	if err != nil {
		return err
	}
	have, err := deploy.ShmSize(ctx, PostgresContainer)
	if err != nil || have < 0 || have == want {
		return err
	}
	panellog.Infof("postgres: resizing /dev/shm from %d MB to %d MB, restarting it", have>>20, want>>20)
	if err := deploy.ResizeShm(ctx, PostgresContainer, want, 2*time.Minute); err != nil {
		return fmt.Errorf("resizing postgres's /dev/shm: %w", err)
	}
	if !deploy.WaitHealthy(60, time.Second, func() bool { return deploy.PostgresReady(ctx, PostgresContainer) }) {
		return fmt.Errorf("postgres failed to become ready after resizing /dev/shm")
	}
	// ALTER SYSTEM lives in the volume: a type set there wouldn't use the new size.
	if rows, err := deploy.PostgresQuery(ctx, PostgresContainer, "SHOW dynamic_shared_memory_type"); err == nil && len(rows) == 1 && len(rows[0]) == 1 && rows[0][0] != "posix" {
		panellog.Warnf("postgres: dynamic_shared_memory_type is %s (set by ALTER SYSTEM?), so the larger /dev/shm goes unused", rows[0][0])
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
			panellog.Error("failed to drop", scratch+":", err)
		}
	}, nil
}

// replaceWith swaps the database's content for a dump: the dump is
// restored into a scratch database first, so a broken one changes
// nothing, then renamed over it. Whatever is connected to the database is
// cut off.
func replaceWith(ctx context.Context, d DBSpec, dump io.Reader) error {
	// Database names can't contain dots, so this never clashes with one.
	scratch := "hakobu.restore." + d.Name
	drop, err := scratchDatabase(ctx, d, scratch)
	if err != nil {
		return err
	}
	if err := backup.RestoreDatabase(ctx, PostgresContainer, d.User, scratch, dump); err != nil {
		drop()
		return err
	}
	for _, sql := range []string{
		fmt.Sprintf(`DROP DATABASE IF EXISTS "%s" WITH (FORCE)`, d.Name),
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
	for _, v := range volumes {
		if err := n.RemoveVolume(ctx, app, v); err != nil {
			return err
		}
	}
	return nil
}
