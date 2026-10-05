package ops

import (
	"testing"

	"github.com/x0ryz/hakobu/internal/panellog"
	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store/teldb"
)

func TestPanelLog(t *testing.T) {
	s := notifyStore(t)
	panellog.SetSink(func(l panellog.Level, msg string) {
		_ = s.Tel.AddPanelLog(ctx(), teldb.AddPanelLogParams{Level: int64(l), Message: secret.String(msg)})
	})
	t.Cleanup(func() { panellog.SetSink(nil) })
	panellog.Info("server box connected")
	panellog.Warn("tunnel routes not updated (retrying):", "timeout")
	panellog.Error("backup failed for main:", "no bucket")

	for level, want := range map[panellog.Level][]string{
		panellog.LevelInfo:  {"error backup failed for main: no bucket", "warn tunnel routes not updated (retrying): timeout", "info server box connected"},
		panellog.LevelWarn:  {"error backup failed for main: no bucket", "warn tunnel routes not updated (retrying): timeout"},
		panellog.LevelError: {"error backup failed for main: no bucket"},
	} {
		lines, err := PanelLog(s, level, 10)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, l := range lines {
			got = append(got, l.Level+" "+l.Message)
		}
		if len(got) != len(want) {
			t.Errorf("level %s: %q, want %q", level, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("level %s: %q, want %q", level, got, want)
			}
		}
	}
	if n := PanelErrorsToday(s); n != 1 {
		t.Errorf("%d errors today, want 1", n)
	}
}
