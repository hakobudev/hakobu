package ops

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/hakobudev/hakobu/internal/cloudflare"
	"github.com/hakobudev/hakobu/internal/store"
)

// The watchdog records in D1 whether each target answered from outside,
// minute by minute, including while the server was down and hakobu
// couldn't record anything itself.

// Uptime is a target's record from outside.
type Uptime struct {
	Windows []UptimeWindow
	AvgMs   int64 // over the last 24 hours
	Outages []Outage
	Last    time.Time   // the latest check
	Days    []UptimeDay // the last 30 days, oldest first, today last
}

// UptimeDay is one UTC day of checks.
type UptimeDay struct {
	Day    time.Time
	Pct    float64
	Checks int64
}

// uptimeDays is how many days the day by day record covers.
const uptimeDays = 30

// UptimeWindow is the share of minutes the target answered in a span.
type UptimeWindow struct {
	Label  string
	Pct    float64
	Checks int64
}

// Outage is a run of minutes the target didn't answer.
type Outage struct {
	Start, End time.Time // End is the last minute down
	Status     int64     // the answer at its end, 0 for none
	Ongoing    bool
}

// Minutes is how long it lasted.
func (o Outage) Minutes() int { return int(o.End.Sub(o.Start).Minutes()) + 1 }

// ErrNoHistory: the watchdog keeps no history, being off or without D1.
var ErrNoHistory = errors.New("no history")

var uptimeCache struct {
	sync.Mutex
	at  map[string]time.Time
	val map[string]Uptime
}

// uptimeFresh is how long a read of the history holds.
const uptimeFresh = 2 * time.Minute

// maxOutages is how many of the latest outages are shown.
const maxOutages = 10

// UptimeOf reads target's record ("panel" or AppTarget) from the
// watchdog's history.
func UptimeOf(s *store.Store, target string) (Uptime, error) {
	w, err := s.GetWatchdog(ctx())
	if err != nil || w.D1DatabaseID == "" {
		return Uptime{}, ErrNoHistory
	}
	uptimeCache.Lock()
	if time.Since(uptimeCache.at[target]) < uptimeFresh {
		defer uptimeCache.Unlock()
		return uptimeCache.val[target], nil
	}
	uptimeCache.Unlock()

	c, cf, err := cfClient(s)
	if err != nil {
		return Uptime{}, err
	}
	now := time.Now()
	windows := []struct {
		label string
		span  time.Duration
	}{{"24 hours", 24 * time.Hour}, {"7 days", 7 * 24 * time.Hour}, {"30 days", 30 * 24 * time.Hour}}
	var queries []cloudflare.D1Query
	for _, win := range windows {
		queries = append(queries, cloudflare.D1Query{
			SQL:    "SELECT count(*) AS n, coalesce(sum(up), 0) AS up, coalesce(avg(ms), 0) AS ms FROM checks WHERE target = ? AND ts >= ?",
			Params: []any{target, now.Add(-win.span).Unix()},
		})
	}
	firstDay := dayOf(now).AddDate(0, 0, -(uptimeDays - 1))
	downQ, lastQ, daysQ := len(queries), len(queries)+1, len(queries)+2
	queries = append(queries,
		cloudflare.D1Query{SQL: "SELECT ts, status FROM checks WHERE target = ? AND up = 0 AND ts >= ? ORDER BY ts DESC LIMIT 5000", Params: []any{target, now.Add(-30 * 24 * time.Hour).Unix()}},
		cloudflare.D1Query{SQL: "SELECT coalesce(max(ts), 0) AS ts FROM checks WHERE target = ?", Params: []any{target}},
		cloudflare.D1Query{SQL: "SELECT ts / 86400 AS day, count(*) AS n, coalesce(sum(up), 0) AS up FROM checks WHERE target = ? AND ts >= ? GROUP BY day", Params: []any{target, firstDay.Unix()}},
	)
	res, err := c.QueryD1(cf.AccountID, w.D1DatabaseID, queries...)
	if err != nil {
		return Uptime{}, err
	}
	if len(res) != len(queries) {
		return Uptime{}, fmt.Errorf("D1 answered %d of %d queries", len(res), len(queries))
	}
	var u Uptime
	for i, win := range windows {
		var n, up, ms float64
		if len(res[i]) == 1 {
			n, up, ms = number(res[i][0]["n"]), number(res[i][0]["up"]), number(res[i][0]["ms"])
		}
		uw := UptimeWindow{Label: win.label, Checks: int64(n)}
		if n > 0 {
			uw.Pct = up / n * 100
		}
		u.Windows = append(u.Windows, uw)
		if i == 0 {
			u.AvgMs = int64(ms)
		}
	}
	if len(res[lastQ]) == 1 {
		if ts := int64(number(res[lastQ][0]["ts"])); ts > 0 {
			u.Last = time.Unix(ts, 0)
		}
	}
	var down []struct{ ts, status int64 }
	for _, r := range res[downQ] {
		down = append(down, struct{ ts, status int64 }{int64(number(r["ts"])), int64(number(r["status"]))})
	}
	u.Outages = outages(down, u.Last)
	var perDay []dayChecks
	for _, r := range res[daysQ] {
		perDay = append(perDay, dayChecks{int64(number(r["day"])), int64(number(r["n"])), int64(number(r["up"]))})
	}
	u.Days = days(perDay, now)

	uptimeCache.Lock()
	if uptimeCache.at == nil {
		uptimeCache.at, uptimeCache.val = map[string]time.Time{}, map[string]Uptime{}
	}
	uptimeCache.at[target], uptimeCache.val[target] = time.Now(), u
	uptimeCache.Unlock()
	return u, nil
}

// outages groups the minutes down, newest first, into runs: minutes a
// check apart (two, as a run may miss one) are one outage.
func outages(down []struct{ ts, status int64 }, last time.Time) []Outage {
	var out []Outage
	for _, d := range down {
		t := time.Unix(d.ts, 0)
		if n := len(out); n > 0 && out[n-1].Start.Sub(t) <= 2*time.Minute {
			out[n-1].Start = t
			continue
		}
		if len(out) == maxOutages {
			break
		}
		out = append(out, Outage{Start: t, End: t, Status: d.status, Ongoing: !last.IsZero() && t.Equal(last)})
	}
	return out
}

// dayChecks is one UTC day's count of checks and of those answered.
type dayChecks struct{ day, n, up int64 }

// days lays the counts out over the last uptimeDays days, a day with no
// checks left at zero.
func days(rows []dayChecks, now time.Time) []UptimeDay {
	byDay := map[int64]dayChecks{}
	for _, r := range rows {
		byDay[r.day] = r
	}
	out := make([]UptimeDay, uptimeDays)
	first := dayOf(now).AddDate(0, 0, -(uptimeDays - 1))
	for i := range out {
		d := first.AddDate(0, 0, i)
		out[i].Day = d
		if r, ok := byDay[d.Unix()/86400]; ok && r.n > 0 {
			out[i].Checks, out[i].Pct = r.n, float64(r.up)/float64(r.n)*100
		}
	}
	return out
}

// dayOf is the start of t's day in UTC.
func dayOf(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func number(raw json.RawMessage) float64 {
	var f float64
	_ = json.Unmarshal(raw, &f)
	return f
}
