package ops

import (
	"fmt"
	"time"

	"github.com/hakobudev/hakobu/internal/panellog"
	"github.com/hakobudev/hakobu/internal/store"
	"github.com/hakobudev/hakobu/internal/store/teldb"
)

// A database's or cache's record is the panel's own checks of it
// (checkData), kept in telemetry.db: the watchdog can't reach them from
// outside, and shouldn't. A time its server wasn't connected has no
// checks, so it counts neither way.

// DBTarget is what a database's checks are recorded as.
func DBTarget(id int64) string { return fmt.Sprintf("db:%d", id) }

// recordDataCheck keeps one check of target.
func recordDataCheck(s *store.Store, target string, at time.Time, h DataHealth) {
	up := int64(0)
	if h.Answering {
		up = 1
	}
	if err := s.Tel.PutDataCheck(ctx(), teldb.PutDataCheckParams{Target: target, Ts: at.Unix(), Up: up, Why: h.Why}); err != nil {
		panellog.Warn("failed to keep the check of", target+":", err)
	}
}

// dataUptime is target's record over the windows, days and outages the
// watchdog's has.
func dataUptime(s *store.Store, target string) (Uptime, error) {
	now := time.Now()
	var u Uptime
	for _, win := range []struct {
		label string
		span  time.Duration
	}{{"24 hours", 24 * time.Hour}, {"7 days", 7 * 24 * time.Hour}, {"30 days", 30 * 24 * time.Hour}} {
		r, err := s.Tel.DataCheckWindow(ctx(), teldb.DataCheckWindowParams{Target: target, Ts: now.Add(-win.span).Unix()})
		if err != nil {
			return Uptime{}, err
		}
		w := UptimeWindow{Label: win.label, Checks: r.N}
		if r.N > 0 {
			w.Pct = float64(r.Up) / float64(r.N) * 100
		}
		u.Windows = append(u.Windows, w)
	}
	last, err := s.Tel.LastDataCheck(ctx(), target)
	if err != nil {
		return Uptime{}, err
	}
	if last > 0 {
		u.Last = time.Unix(last, 0)
	}
	rows, err := s.Tel.DataChecksDown(ctx(), teldb.DataChecksDownParams{Target: target, Ts: now.Add(-30 * 24 * time.Hour).Unix()})
	if err != nil {
		return Uptime{}, err
	}
	down := make([]downCheck, len(rows))
	for i, r := range rows {
		down[i] = downCheck{ts: r.Ts, why: r.Why}
	}
	u.Outages = outages(down, u.Last)
	perDay, err := s.Tel.DataChecksByDay(ctx(), teldb.DataChecksByDayParams{Target: target, Ts: dayOf(now).AddDate(0, 0, -(uptimeDays - 1)).Unix()})
	if err != nil {
		return Uptime{}, err
	}
	dc := make([]dayChecks, len(perDay))
	for i, r := range perDay {
		dc[i] = dayChecks{r.Day, r.N, r.Up}
	}
	u.Days = days(dc, now)
	return u, nil
}
