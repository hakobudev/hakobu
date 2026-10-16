package ops

import (
	"testing"
	"time"
)

// Minutes down a check apart, or two (a run may miss one), are one outage.
func TestOutages(t *testing.T) {
	m := func(minute int64) int64 { return 1_790_000_000 + minute*60 }
	down := []downCheck{ // newest first
		{ts: m(100), status: 530}, {ts: m(99), status: 530}, {ts: m(97), status: 530}, // one, still going
		{ts: m(50)}, // one minute
		{ts: m(10), status: 502}, {ts: m(9), status: 502},
	}
	got := outages(down, time.Unix(m(100), 0))
	if len(got) != 3 {
		t.Fatalf("outages %+v", got)
	}
	if !got[0].Ongoing || got[0].Minutes() != 4 || got[0].Status != 530 {
		t.Errorf("latest %+v, %d min", got[0], got[0].Minutes())
	}
	if got[1].Ongoing || got[1].Minutes() != 1 || got[2].Minutes() != 2 {
		t.Errorf("older %+v", got[1:])
	}
}

// Days cover the last 30, today last; a day with no checks stays empty.
func TestDays(t *testing.T) {
	now := time.Date(2026, 10, 11, 15, 0, 0, 0, time.UTC)
	today := dayOf(now).Unix() / 86400
	got := days([]dayChecks{{today, 1440, 1440}, {today - 8, 1440, 1436}, {today - 40, 10, 10}}, now)
	if len(got) != 30 {
		t.Fatalf("%d days", len(got))
	}
	if !got[29].Day.Equal(dayOf(now)) || got[29].Pct != 100 {
		t.Errorf("today %+v", got[29])
	}
	if d := got[21]; d.Checks != 1440 || d.Pct > 99.73 || d.Pct < 99.72 {
		t.Errorf("8 days ago %+v", d)
	}
	if got[0].Checks != 0 || !got[0].Day.Equal(dayOf(now).AddDate(0, 0, -29)) {
		t.Errorf("first %+v", got[0])
	}
}
