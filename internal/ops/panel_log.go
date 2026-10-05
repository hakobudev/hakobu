package ops

import (
	"time"

	"github.com/x0ryz/hakobu/internal/panellog"
	"github.com/x0ryz/hakobu/internal/store"
	"github.com/x0ryz/hakobu/internal/store/teldb"
)

// PanelLogLine is a line of the panel's own log (internal/panellog).
type PanelLogLine struct {
	At, Level, Message string
}

// PanelLog is the panel's log at level and above, newest first, at most
// limit lines.
func PanelLog(s *store.Store, level panellog.Level, limit int) ([]PanelLogLine, error) {
	rows, err := s.Tel.ListPanelLog(ctx(), teldb.ListPanelLogParams{Level: int64(level), Limit: int64(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]PanelLogLine, len(rows))
	for i, r := range rows {
		out[i] = PanelLogLine{At: r.CreatedAt, Level: panellog.Level(r.Level).String(), Message: string(r.Message)}
	}
	return out, nil
}

// PanelErrorsToday counts the panel's errors in the last 24 hours.
func PanelErrorsToday(s *store.Store) int64 {
	n, _ := s.Tel.CountPanelErrorsSince(ctx(), time.Now().Add(-24*time.Hour).UTC().Format("2006-01-02T15:04:05Z"))
	return n
}
