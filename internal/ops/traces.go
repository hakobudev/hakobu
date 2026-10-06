package ops

import (
	"fmt"
	"slices"
	"time"

	"github.com/x0ryz/hakobu/internal/ingest"
	"github.com/x0ryz/hakobu/internal/node"
	"github.com/x0ryz/hakobu/internal/store"
	"github.com/x0ryz/hakobu/internal/store/teldb"
)

// Apps' traces are counted and kept on their server (node.Telemetry);
// these read them.

// RouteStats is how a route of an app did over a span.
type RouteStats struct {
	Name          string
	Count, Errors int64
	TotalMs       int64 // all its requests together: what it costs
	AvgMs         int64
	P50Ms, P95Ms  int64
	P95Over       bool // P95Ms is the last bound: it took longer
}

// ErrorPct is the share of requests that failed, in percent.
func (r RouteStats) ErrorPct() float64 {
	if r.Count == 0 {
		return 0
	}
	return float64(r.Errors) / float64(r.Count) * 100
}

// RoutesOf sums an app's routes over the span, the ones that took the
// most time in all first.
func RoutesOf(s *store.Store, app string, span time.Duration) ([]RouteStats, error) {
	t, err := appTelemetry(s, app)
	if err != nil {
		return nil, err
	}
	rows, err := t.TraceRoutes(ctx(), app, time.Now().Add(-span).Truncate(time.Hour).Unix())
	if err != nil {
		return nil, err
	}
	type sum struct {
		count, errors, total int64
		buckets              [len(node.BucketBounds) + 1]int64
	}
	byName := map[string]*sum{}
	for _, r := range rows {
		t := byName[r.Name]
		if t == nil {
			t = &sum{}
			byName[r.Name] = t
		}
		t.count += r.Count
		t.errors += r.Errors
		t.total += r.TotalMs
		for i, b := range []int64{r.B0, r.B1, r.B2, r.B3, r.B4, r.B5, r.B6, r.B7, r.B8, r.B9, r.B10} {
			t.buckets[i] += b
		}
	}
	out := make([]RouteStats, 0, len(byName))
	for name, t := range byName {
		if t.count == 0 {
			continue
		}
		r := RouteStats{Name: name, Count: t.count, Errors: t.errors, TotalMs: t.total, AvgMs: t.total / t.count}
		r.P50Ms, _ = percentile(t.buckets[:], t.count, 0.5)
		r.P95Ms, r.P95Over = percentile(t.buckets[:], t.count, 0.95)
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b RouteStats) int {
		if a.TotalMs != b.TotalMs {
			return int(b.TotalMs - a.TotalMs)
		}
		return int(b.Count - a.Count)
	})
	return out, nil
}

// percentile estimates the q-th duration from the histogram, spread evenly
// within its bucket; over is true when it's in the last, unbounded one.
func percentile(buckets []int64, count int64, q float64) (ms int64, over bool) {
	rank := q * float64(count)
	var seen float64
	for i, n := range buckets {
		if n == 0 || seen+float64(n) < rank {
			seen += float64(n)
			continue
		}
		if i == len(node.BucketBounds) {
			return node.BucketBounds[len(node.BucketBounds)-1], true
		}
		lower := int64(0)
		if i > 0 {
			lower = node.BucketBounds[i-1]
		}
		frac := (rank - seen) / float64(n)
		return lower + int64(frac*float64(node.BucketBounds[i]-lower)), false
	}
	return 0, false
}

// Waterfall is a kept trace with its spans laid out over its duration.
type Waterfall struct {
	teldb.Trace
	Tx   ingest.Transaction
	Rows []WaterfallRow
}

// WaterfallRow is the transaction itself or one of its spans.
type WaterfallRow struct {
	Op, Description, Status string
	Depth                   int
	Ms                      float64
	LeftPct, WidthPct       float64
}

// TraceOf reads a kept trace of app and lays it out.
func TraceOf(s *store.Store, app string, id int64) (Waterfall, error) {
	keeper, err := appTelemetry(s, app)
	if err != nil {
		return Waterfall{}, err
	}
	t, err := keeper.AppTrace(ctx(), app, id)
	if err != nil {
		return Waterfall{}, fmt.Errorf("no trace %d of %s (traces are kept for a week)", id, app)
	}
	tx, ok := ingest.ExtractTransaction(ingest.Item{Type: "transaction", Payload: []byte(t.Payload)})
	if !ok {
		return Waterfall{}, fmt.Errorf("trace %d of %s can't be read", id, app)
	}
	return Waterfall{Trace: t, Tx: tx, Rows: waterfallRows(tx)}, nil
}

// maxWaterfallRows caps the spans shown; SDKs send up to 1000.
const maxWaterfallRows = 300

func waterfallRows(tx ingest.Transaction) []WaterfallRow {
	total := float64(max(tx.Duration, time.Microsecond))
	pct := func(d time.Duration) float64 { return min(max(float64(d)/total*100, 0), 100) }
	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
	rows := []WaterfallRow{{Op: tx.Op, Description: tx.Name, Status: tx.Status, Ms: ms(tx.Duration), WidthPct: 100}}
	depth := map[string]int{}
	for _, sp := range tx.Spans {
		d := 1
		if p, ok := depth[sp.Parent]; ok {
			d = p + 1
		}
		depth[sp.ID] = d
		if len(rows) > maxWaterfallRows {
			continue
		}
		left := pct(sp.Start)
		rows = append(rows, WaterfallRow{
			Op: sp.Op, Description: sp.Description, Status: sp.Status, Depth: min(d, 8), Ms: ms(sp.Duration),
			LeftPct: left, WidthPct: max(min(pct(sp.Duration), 100-left), 0.3),
		})
	}
	return rows
}
