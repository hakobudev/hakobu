package node

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hakobudev/hakobu/internal/deploy"
)

// DataCheckTimeout is how long a database or cache may take to answer its
// minute's check before it counts as not answering.
var DataCheckTimeout = 20 * time.Second

// CacheProbe is what a cache's check needs: its container and password.
type CacheProbe struct {
	Name     string
	Password string
}

// DataStatus is how a node's databases and caches answer now.
type DataStatus struct {
	// Postgres is why the shared Postgres doesn't answer, "" if it does
	// (or wasn't asked: no databases).
	Postgres string
	// The shared Postgres's version, and its /dev/shm in bytes with the
	// size FitPostgres gives it ("" and 0 from a node before v0.30.2).
	PostgresVersion   string
	PostgresShm       int64
	PostgresShmWanted int64
	Databases         map[string]DBStatus // by DBSpec.Name
	Caches            map[string]string   // why each doesn't answer, "" if it does
}

// DBStatus is a database as Postgres reports it.
type DBStatus struct {
	Missing     bool  // Postgres answers but has no such database
	SizeBytes   int64 // on disk
	Connections int64 // open now
}

// DataStatus asks the shared Postgres about the databases named (one
// query for all) and PINGs each cache, every one within
// DataCheckTimeout.
func (Local) DataStatus(ctx context.Context, databases []string, caches []CacheProbe) (DataStatus, error) {
	st := DataStatus{Databases: map[string]DBStatus{}, Caches: map[string]string{}}
	var wg sync.WaitGroup
	var mu sync.Mutex
	if len(databases) > 0 {
		wg.Go(func() {
			why, version, dbs := postgresStatus(ctx, databases)
			shm, _ := deploy.ShmSize(ctx, PostgresContainer)
			if shm == 0 {
				shm = defaultShm
			}
			wanted, _ := postgresShm()
			mu.Lock()
			defer mu.Unlock()
			st.Postgres, st.PostgresVersion = why, version
			if shm > 0 && wanted > 0 {
				st.PostgresShm, st.PostgresShmWanted = shm, wanted
			}
			for name, d := range dbs {
				st.Databases[name] = d
			}
		})
	}
	for _, c := range caches {
		wg.Go(func() {
			why := cacheStatus(ctx, c)
			mu.Lock()
			defer mu.Unlock()
			st.Caches[c.Name] = why
		})
	}
	wg.Wait()
	return st, nil
}

// defaultShm is the /dev/shm Docker gives a container made without a size.
const defaultShm = 64 << 20

func postgresStatus(ctx context.Context, databases []string) (why, version string, dbs map[string]DBStatus) {
	ctx, cancel := context.WithTimeout(ctx, DataCheckTimeout)
	defer cancel()
	rows, err := deploy.PostgresQuery(ctx, PostgresContainer, `SELECT d.datname, pg_database_size(d.datname), coalesce(s.numbackends, 0), current_setting('server_version')
		FROM pg_database d LEFT JOIN pg_stat_database s ON s.datid = d.oid WHERE NOT d.datistemplate`)
	if err != nil {
		return answerFailure(ctx, err), "", nil
	}
	found := map[string]DBStatus{}
	for _, r := range rows {
		if len(r) != 4 {
			continue
		}
		// "18.6 (Debian 18.6-1.pgdg13+1)": the number is enough.
		version, _, _ = strings.Cut(r[3], " ")
		size, _ := strconv.ParseInt(r[1], 10, 64)
		conns, _ := strconv.ParseInt(r[2], 10, 64)
		found[r[0]] = DBStatus{SizeBytes: size, Connections: conns}
	}
	dbs = map[string]DBStatus{}
	for _, name := range databases {
		d, ok := found[name]
		d.Missing = !ok
		dbs[name] = d
	}
	return "", version, dbs
}

func cacheStatus(ctx context.Context, c CacheProbe) string {
	ctx, cancel := context.WithTimeout(ctx, DataCheckTimeout)
	defer cancel()
	if deploy.ValkeyReady(ctx, c.Name, c.Password) {
		return ""
	}
	if ctx.Err() != nil {
		return "didn't answer within " + DataCheckTimeout.String()
	}
	if st := deploy.ContainerState(context.Background(), c.Name); st.Status != "running" {
		return "isn't running (" + st.Status + ")"
	}
	return "doesn't answer PING"
}

// answerFailure says why a check got no answer: too slow, the container
// not running, or what failed.
func answerFailure(ctx context.Context, err error) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "didn't answer within " + DataCheckTimeout.String()
	}
	if st := deploy.ContainerState(context.Background(), PostgresContainer); st.Status != "running" {
		return "isn't running (" + st.Status + ")"
	}
	msg := err.Error()
	if i := strings.IndexByte(msg, '\n'); i > 0 {
		msg = msg[:i]
	}
	return "fails: " + msg
}

// VolumeSizes is the size in bytes of each app volume on the node, by
// Volume name.
func (Local) VolumeSizes(ctx context.Context) (map[string]int64, error) {
	return deploy.VolumeSizes(ctx, volumePrefix)
}
