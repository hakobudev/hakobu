package ops

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/hakobudev/hakobu/internal/config"
	"github.com/hakobudev/hakobu/internal/node"
	"github.com/hakobudev/hakobu/internal/panellog"
	"github.com/hakobudev/hakobu/internal/store"
)

// Databases and caches are asked on their servers whether they answer, as
// often as apps' health checks run; one that fails healthFailures checks
// in a row is down, and its owner is emailed then and when it answers
// again. A server that isn't connected says nothing: that's its own alert.
// The same checks read the databases' sizes and connections, and every
// hour the volumes' sizes, for the project's cards.

// DataHealth is the latest check of a database or cache.
type DataHealth struct {
	At          time.Time
	Answering   bool
	Why         string // why it doesn't answer
	Down        bool   // it has failed healthFailures checks in a row
	SizeBytes   int64  // a database's
	Connections int64  // a database's
}

// Fresh reports whether the check is recent enough to show: an older one
// is from before its server went away.
func (h DataHealth) Fresh() bool {
	return !h.At.IsZero() && time.Since(h.At) < 3*config.HealthCheckEvery+time.Minute
}

// volumeSizesEvery is how often the volumes' sizes are read: Docker walks
// every file of them for it.
const volumeSizesEvery = time.Hour

var dataHealth struct {
	sync.Mutex
	dbs     map[int64]DataHealth // by database ID
	caches  map[int64]DataHealth // by cache ID
	failed  map[string]int       // checks failed in a row, by alert key
	volumes map[string]int64     // bytes, by volumeKey
}

func volumeKey(appID int64, name string) string { return strconv.FormatInt(appID, 10) + "/" + name }

// DatabaseHealth is the latest check of database id.
func DatabaseHealth(id int64) (DataHealth, bool) {
	dataHealth.Lock()
	defer dataHealth.Unlock()
	h, ok := dataHealth.dbs[id]
	return h, ok && h.Fresh()
}

// CacheHealth is the latest check of cache id.
func CacheHealth(id int64) (DataHealth, bool) {
	dataHealth.Lock()
	defer dataHealth.Unlock()
	h, ok := dataHealth.caches[id]
	return h, ok && h.Fresh()
}

// VolumeSize is the size of the app's volume at its last count.
func VolumeSize(appID int64, name string) (int64, bool) {
	dataHealth.Lock()
	defer dataHealth.Unlock()
	n, ok := dataHealth.volumes[volumeKey(appID, name)]
	return n, ok
}

// WatchData checks the databases and caches, and counts the volumes.
func WatchData(s *store.Store) {
	var counted time.Time
	for {
		checkData(s)
		if time.Since(counted) >= volumeSizesEvery {
			countVolumes(s)
			counted = time.Now()
		}
		time.Sleep(config.HealthCheckEvery)
	}
}

// checkData asks each connected server about its databases and caches.
func checkData(s *store.Store) {
	dbs, err := s.ListDatabases(ctx())
	if err != nil {
		return
	}
	caches, err := s.ListCaches(ctx())
	if err != nil {
		return
	}
	places, err := projectPlaces(s)
	if err != nil {
		return
	}
	var wg sync.WaitGroup
	for _, pn := range allNodes(s) {
		var mine []store.Database
		var names []string
		for _, d := range dbs {
			if places[d.ProjectID].server == pn.ID {
				mine, names = append(mine, d), append(names, d.Key())
			}
		}
		var myCaches []store.Cache
		var probes []node.CacheProbe
		for _, c := range caches {
			if places[c.ProjectID].server == pn.ID {
				myCaches = append(myCaches, c)
				probes = append(probes, node.CacheProbe{Name: c.Key(), Password: string(c.Password)})
			}
		}
		if len(names) == 0 && len(probes) == 0 {
			continue
		}
		wg.Go(func() {
			c, cancel := context.WithTimeout(ctx(), 2*node.DataCheckTimeout)
			defer cancel()
			st, err := pn.n.DataStatus(c, names, probes)
			if err != nil {
				panellog.Warn("databases and caches of", pn.label()+":", err)
				return
			}
			now := time.Now()
			for _, d := range mine {
				h := DataHealth{At: now, Answering: true}
				switch r := st.Databases[d.Key()]; {
				case st.Postgres != "":
					h.Answering, h.Why = false, "Postgres "+st.Postgres
				case r.Missing:
					h.Answering, h.Why = false, "is missing from Postgres"
				default:
					h.SizeBytes, h.Connections = r.SizeBytes, r.Connections
				}
				noteDatabase(s, d, h)
			}
			for _, c := range myCaches {
				why, ok := st.Caches[c.Key()]
				if !ok {
					continue
				}
				noteCache(s, c, DataHealth{At: now, Answering: why == "", Why: why})
			}
		})
	}
	wg.Wait()
	forgetData(dbs, caches)
}

func noteDatabase(s *store.Store, d store.Database, h DataHealth) {
	key := fmt.Sprintf("dbdown:%d", d.ID)
	h.Down = countData(key, h.Answering)
	dataHealth.Lock()
	if dataHealth.dbs == nil {
		dataHealth.dbs = map[int64]DataHealth{}
	}
	dataHealth.dbs[d.ID] = h
	dataHealth.Unlock()
	alertData(s, key, "Database "+d.Name, h, panelURL(DatabasePath(s, d)))
}

func noteCache(s *store.Store, c store.Cache, h DataHealth) {
	key := fmt.Sprintf("cachedown:%d", c.ID)
	h.Down = countData(key, h.Answering)
	dataHealth.Lock()
	if dataHealth.caches == nil {
		dataHealth.caches = map[int64]DataHealth{}
	}
	dataHealth.caches[c.ID] = h
	dataHealth.Unlock()
	alertData(s, key, "Cache "+c.Name, h, panelURL(CachePath(s, c)))
}

// countData counts a check of key and reports whether it's down: failed
// healthFailures checks in a row.
func countData(key string, answering bool) bool {
	dataHealth.Lock()
	defer dataHealth.Unlock()
	if dataHealth.failed == nil {
		dataHealth.failed = map[string]int{}
	}
	if answering {
		delete(dataHealth.failed, key)
		return false
	}
	dataHealth.failed[key]++
	return dataHealth.failed[key] >= healthFailures
}

// alertData mails the owner when what goes down, and once it's back.
func alertData(s *store.Store, key, what string, h DataHealth, page string) {
	if h.Down {
		problem(s, key, notifyAgain, what+": not answering",
			fmt.Sprintf("%s %s, %d checks in a row: apps using it fail.\n\n%s", what, h.Why, healthFailures, page))
	} else if h.Answering {
		solved(s, key, what+": answering again", what+" answers again.\n\n"+page)
	}
}

// forgetData drops what was deleted.
func forgetData(dbs []store.Database, caches []store.Cache) {
	liveDBs, liveCaches := map[int64]bool{}, map[int64]bool{}
	for _, d := range dbs {
		liveDBs[d.ID] = true
	}
	for _, c := range caches {
		liveCaches[c.ID] = true
	}
	dataHealth.Lock()
	defer dataHealth.Unlock()
	for id := range dataHealth.dbs {
		if !liveDBs[id] {
			delete(dataHealth.dbs, id)
			delete(dataHealth.failed, fmt.Sprintf("dbdown:%d", id))
		}
	}
	for id := range dataHealth.caches {
		if !liveCaches[id] {
			delete(dataHealth.caches, id)
			delete(dataHealth.failed, fmt.Sprintf("cachedown:%d", id))
		}
	}
}

// countVolumes reads the size of every app volume on the connected
// servers; a server is believed only about the volumes of its own apps.
func countVolumes(s *store.Store) {
	vols, err := s.ListAllVolumes(ctx())
	if err != nil || len(vols) == 0 {
		return
	}
	apps, err := s.ListApps(ctx())
	if err != nil {
		return
	}
	places, err := projectPlaces(s)
	if err != nil {
		return
	}
	serverOf := map[int64]int64{} // app ID → server ID
	for _, a := range apps {
		serverOf[a.ID] = places[a.ProjectID].server
	}
	out := map[string]int64{}
	for _, pn := range allNodes(s) {
		got, err := pn.n.VolumeSizes(ctx())
		if err != nil {
			panellog.Warn("volume sizes of", pn.label()+":", err)
			continue
		}
		for _, v := range vols {
			if serverOf[v.AppID] != pn.ID {
				continue
			}
			if n, ok := got[node.Volume(store.AppKey(v.AppID), v.Name)]; ok {
				out[volumeKey(v.AppID, v.Name)] = n
			}
		}
	}
	dataHealth.Lock()
	dataHealth.volumes = out
	dataHealth.Unlock()
}
