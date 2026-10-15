package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hakobudev/hakobu/internal/ops"
	"github.com/hakobudev/hakobu/internal/store"
	"github.com/hakobudev/hakobu/internal/store/teldb"
)

// The panel draws usage as SVG made here: no chart library to load.

const (
	chartW = 600
	chartH = 110
)

// series is one line of a chart: the averages, and the peaks shaded
// behind them when they differ.
type series struct {
	name  string
	color string
	avg   []float64
	peak  []float64
}

// chart is a usage chart with its numbers.
type chart struct {
	Title    string
	Now      string // the latest value, with the limit if any
	Peak     string // the highest over the span
	SVG      template.HTML
	YTicks   [3]string // the scale's top, middle and bottom
	From, To int64     // the span, for the time axis
	Points   string    // JSON for the pointer's readout: {"t":[…],"v":[[…],…]}
	Legend   []series  // its series by name and color, when there are several
	Clipped  []float64 // where peaks go over the scale, as % of the width
}

// bucketed groups the samples into at most n runs of equal length, so a
// day of minutes draws as a calm line: each run's average, with its
// highest as the peak shaded above it. A run starts a gap where its
// samples do.
func bucketed(ts []int64, res int64, lines []series, n int) ([]int64, int64, []series) {
	k := (len(ts) + n - 1) / n
	if k <= 1 {
		return ts, res, lines
	}
	var outTs []int64
	out := make([]series, len(lines))
	for i := range lines {
		out[i] = series{name: lines[i].name, color: lines[i].color}
	}
	for start := 0; start < len(ts); {
		end := min(start+k, len(ts))
		for j := start + 1; j < end; j++ { // don't bridge a gap
			if ts[j]-ts[j-1] > 2*res {
				end = j
				break
			}
		}
		outTs = append(outTs, ts[start])
		for i, l := range lines {
			if end > len(l.avg) {
				continue
			}
			var sum, top float64
			for j := start; j < end; j++ {
				sum += l.avg[j]
				top = max(top, l.avg[j])
				if len(l.peak) == len(l.avg) {
					top = max(top, l.peak[j])
				}
			}
			out[i].avg = append(out[i].avg, sum/float64(end-start))
			out[i].peak = append(out[i].peak, top)
		}
		start = end
	}
	return outTs, res * int64(k), out
}

// percentile is the p-th of all the series' averages.
func percentile(lines []series, p float64) float64 {
	var all []float64
	for _, l := range lines {
		all = append(all, l.avg...)
	}
	if len(all) == 0 {
		return 0
	}
	slices.Sort(all)
	return all[int(p*float64(len(all)-1))]
}

// clippedAt is where any series goes over top, a mark per percent of the
// width at most.
func clippedAt(ts []int64, from, to int64, top float64, lines []series) []float64 {
	seen := map[int]bool{}
	var out []float64
	for _, l := range lines {
		values := l.avg
		if len(l.peak) == len(l.avg) {
			values = l.peak
		}
		for i, v := range values {
			if v <= top || i >= len(ts) {
				continue
			}
			pct := float64(ts[i]-from) / float64(max(to-from, 1)) * 100
			if !seen[int(pct)] {
				seen[int(pct)] = true
				out = append(out, pct)
			}
		}
	}
	slices.Sort(out)
	return out
}

// chartPoints caps how many moments the pointer can read out.
const chartPoints = 360

// usageChart draws the series over [from, to]; ts are the samples' times
// and res their spacing, so missing samples leave a gap. A zero-based
// chart starts at 0; another spans the values' own range, so memory that
// moves by a few MB shows it. The limit is drawn when it's near enough to
// show without flattening the rest.
func usageChart(title string, ts []int64, res, from, to int64, limit float64, zero bool, format func(float64) string, lines ...series) chart {
	c := chart{Title: title, From: from, To: to}
	if len(ts) == 0 {
		return c
	}
	ts, res, lines = bucketed(ts, res, lines, chartPoints)
	lo, hi := math.Inf(1), 0.0
	for _, l := range lines {
		for _, v := range append(slices.Clone(l.avg), l.peak...) {
			lo, hi = min(lo, v), max(hi, v)
		}
	}
	if zero || math.IsInf(lo, 1) {
		lo = 0
	}
	// A spike or two (a deploy, an import) mustn't flatten the rest: a
	// zero-based scale tops out near its 99th percentile, and what goes
	// over is cut at the top and marked.
	clip := 0.0
	if zero {
		if p := percentile(lines, 0.99); p > 0 && hi > p*3 {
			clip, hi = p*1.5, p*1.5
		}
	}
	pad := max((hi-lo)*0.15, hi*0.03)
	if !zero {
		lo = max(0, lo-pad)
	}
	if clip == 0 {
		hi += pad
	}
	showLimit := limit > 0 && limit <= hi*1.3
	if showLimit {
		hi = max(hi, limit*1.05)
	}
	if hi <= lo {
		hi = lo + 1
	}
	c.YTicks = [3]string{format(hi), format((lo + hi) / 2), format(lo)}
	x := func(t int64) float64 { return float64(t-from) / float64(max(to-from, 1)) * chartW }
	y := func(v float64) float64 { return max(0, chartH-(v-lo)/(hi-lo)*chartH) }

	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %d %d" preserveAspectRatio="none" class="chart-svg" role="img" aria-label="%s">`, chartW, chartH, template.HTMLEscapeString(title))
	for _, frac := range []float64{0, 0.5, 1} {
		fmt.Fprintf(&b, `<line x1="0" x2="%d" y1="%.1f" y2="%.1f" style="stroke:var(--border);opacity:.6" stroke-width="1" vector-effect="non-scaling-stroke"/>`, chartW, chartH*frac, chartH*frac)
	}
	segments := func(values []float64) [][]int { // index runs without gaps
		var runs [][]int
		var run []int
		for i := range values {
			if i > 0 && ts[i]-ts[i-1] > 2*res {
				runs, run = append(runs, run), nil
			}
			run = append(run, i)
		}
		return append(runs, run)
	}
	for _, l := range lines {
		if len(l.peak) == len(l.avg) && !slices.Equal(l.peak, l.avg) {
			for _, run := range segments(l.peak) {
				var p strings.Builder
				for _, i := range run {
					fmt.Fprintf(&p, "%.1f,%.1f ", x(ts[i]), y(l.peak[i]))
				}
				for _, i := range slices.Backward(run) {
					fmt.Fprintf(&p, "%.1f,%.1f ", x(ts[i]), y(l.avg[i]))
				}
				fmt.Fprintf(&b, `<polygon points="%s" style="fill:%s" fill-opacity="0.15"/>`, p.String(), l.color)
			}
		}
		for _, run := range segments(l.avg) {
			var p strings.Builder
			for _, i := range run {
				fmt.Fprintf(&p, "%.1f,%.1f ", x(ts[i]), y(l.avg[i]))
			}
			fmt.Fprintf(&b, `<polyline points="%s" fill="none" style="stroke:%s" stroke-width="1.5" vector-effect="non-scaling-stroke"/>`, p.String(), l.color)
		}
	}
	if showLimit {
		fmt.Fprintf(&b, `<line x1="0" x2="%d" y1="%.1f" y2="%.1f" style="stroke:var(--danger)" stroke-dasharray="4 3" stroke-width="1" vector-effect="non-scaling-stroke"/>`, chartW, y(limit), y(limit))
	}
	b.WriteString(`</svg>`)
	c.SVG = template.HTML(b.String())
	if clip > 0 {
		c.Clipped = clippedAt(ts, from, to, clip, lines)
	}
	c.Points = chartReadout(ts, format, lines)
	if len(lines) > 1 {
		c.Legend = lines
	}

	var now, peak []string
	for _, l := range lines {
		name := ""
		if len(lines) > 1 {
			name = l.name + " "
		}
		highest := 0.0
		for _, v := range append(slices.Clone(l.avg), l.peak...) {
			highest = max(highest, v)
		}
		if len(l.avg) == 0 {
			continue
		}
		now = append(now, name+format(l.avg[len(l.avg)-1]))
		peak = append(peak, name+format(highest))
	}
	c.Now = strings.Join(now, " · ")
	if limit > 0 {
		c.Now += " of " + format(limit)
	}
	c.Peak = "peak " + strings.Join(peak, " · ")
	return c
}

// chartReadout is what the pointer reads at up to chartPoints moments:
// each series' value there, with its peak when higher.
func chartReadout(ts []int64, format func(float64) string, lines []series) string {
	step := max(1, (len(ts)+chartPoints-1)/chartPoints)
	type readout struct {
		T []int64    `json:"t"`
		V [][]string `json:"v"`
	}
	var r readout
	for i := 0; i < len(ts); i += step {
		end := min(i+step, len(ts))
		var vals []string
		for _, l := range lines {
			if end > len(l.avg) {
				continue
			}
			var sum, top float64
			for j := i; j < end; j++ {
				sum += l.avg[j]
				top = max(top, l.avg[j])
				if len(l.peak) == len(l.avg) {
					top = max(top, l.peak[j])
				}
			}
			v := sum / float64(end-i)
			text := format(v)
			if len(lines) > 1 {
				text = l.name + " " + text
			}
			if p := format(top); p != format(v) {
				text += " (peak " + p + ")"
			}
			vals = append(vals, text)
		}
		r.T, r.V = append(r.T, ts[i]), append(r.V, vals)
	}
	out, _ := json.Marshal(r)
	return string(out)
}

func cores(v float64) string { return fmt.Sprintf("%.2f CPU", v) }

func bytesText(v float64) string {
	switch {
	case v >= 1<<30:
		return fmt.Sprintf("%.1f GB", v/(1<<30))
	case v >= 1<<20:
		return fmt.Sprintf("%.0f MB", v/(1<<20))
	case v >= 1<<10:
		return fmt.Sprintf("%.0f KB", v/(1<<10))
	}
	return fmt.Sprintf("%.0f B", v)
}

func rateText(v float64) string { return bytesText(v) + "/s" }

func loadText(v float64) string { return fmt.Sprintf("%.2f", v) }

// targetTitle names a target for people.
func targetTitle(target string) string {
	kind, name, _ := strings.Cut(target, ":")
	switch kind {
	case ops.HostTarget:
		return "Server"
	case "app":
		return "App"
	case "worker":
		return "Worker"
	case "cache":
		return "Cache"
	case "service":
		name, _, _ = strings.Cut(name, ":") // service:<name>:<server ID> on other servers
		if name == "postgres" {
			return "PostgreSQL, all databases"
		}
		return name
	}
	return target
}

// targetCharts draws a target's CPU, memory and, for the server, load,
// or else network.
func targetCharts(target string, rows []teldb.Sample, from, to int64) []chart {
	if len(rows) == 0 {
		return nil
	}
	res := int64(60)
	if len(rows) > 1 {
		res = rows[1].Ts - rows[0].Ts
		for i := 2; i < len(rows); i++ {
			res = min(res, rows[i].Ts-rows[i-1].Ts)
		}
	}
	ts := make([]int64, len(rows))
	var cpu, cpuMax, mem, memMax, rx, tx, load []float64
	for i, r := range rows {
		ts[i] = r.Ts
		cpu, cpuMax = append(cpu, r.Cpu), append(cpuMax, r.CpuMax)
		mem, memMax = append(mem, float64(r.Mem)), append(memMax, float64(r.MemMax))
		rx, tx = append(rx, r.NetRx), append(tx, r.NetTx)
		load = append(load, r.Load)
	}
	last := rows[len(rows)-1]
	title := targetTitle(target)
	charts := []chart{
		usageChart(title+" · CPU", ts, res, from, to, last.CpuLimit, true, cores, series{name: "CPU", color: "var(--chart-1)", avg: cpu, peak: cpuMax}),
		usageChart(title+" · memory", ts, res, from, to, float64(last.MemLimit), false, bytesText, series{name: "memory", color: "var(--chart-2)", avg: mem, peak: memMax}),
	}
	if target == ops.HostTarget {
		return append(charts, usageChart(title+" · load", ts, res, from, to, 0, true, loadText, series{name: "load", color: "var(--chart-3)", avg: load}))
	}
	return append(charts, usageChart(title+" · network", ts, res, from, to, 0, true, rateText,
		series{name: "in", color: "var(--chart-1)", avg: rx}, series{name: "out", color: "var(--chart-3)", avg: tx}))
}

// usageRanges are the spans offered, shortest first.
var usageRanges = []string{"1h", "24h", "7d", "30d"}

// usageView is the usage fragment: charts of some targets over a span.
type usageView struct {
	Query  template.URL // the targets, for the range links
	Range  string
	Ranges []string
	Charts []chart
}

// registerUsageRoutes serves GET /usage?target=…&range=…, the charts the
// app, database and settings pages load.
func registerUsageRoutes(handle func(string, http.HandlerFunc), s *store.Store) {
	handle("GET /usage", func(w http.ResponseWriter, r *http.Request) {
		rng := r.URL.Query().Get("range")
		span, ok := ops.MetricRanges[rng]
		if !ok {
			rng, span = "24h", ops.MetricRanges["24h"]
		}
		targets := r.URL.Query()["target"]
		q := url.Values{"target": targets}
		v := usageView{Query: template.URL(q.Encode()), Range: rng, Ranges: usageRanges}
		to := time.Now().Unix()
		from := to - int64(span.Seconds())
		for _, target := range targets {
			rows, err := ops.UsageOf(s, target, span)
			if err != nil {
				fail(w, err)
				return
			}
			v.Charts = append(v.Charts, targetCharts(target, rows, from, to)...)
		}
		renderPage(w, r, usageFragment(v))
	})
}

// registerUptimeRoutes serves GET /uptime?target=…: the watchdog's record
// of a target from outside, for the app and settings pages.
func registerUptimeRoutes(handle func(string, http.HandlerFunc), s *store.Store) {
	handle("GET /uptime", func(w http.ResponseWriter, r *http.Request) {
		u, err := ops.UptimeOf(s, r.URL.Query().Get("target"))
		v := uptimeView{Uptime: u, NoHistory: errors.Is(err, ops.ErrNoHistory)}
		if err != nil && !errors.Is(err, ops.ErrNoHistory) {
			v.Err = err.Error()
		}
		if r.URL.Query().Get("compact") != "" {
			renderPage(w, r, uptimeStrip(v))
			return
		}
		renderPage(w, r, uptimeFragment(v))
	})
}

// percentOf is v as a whole percentage of limit, or -1 without a limit.
func percentOf(v, limit float64) int {
	if limit <= 0 {
		return -1
	}
	return int(math.Round(v / limit * 100))
}

// usageRow is a line of the settings' table of what uses the server now.
type usageRow struct {
	Name, Link  string
	CPU, Memory string
	MemPct      int // of its limit, -1 without one
}

// usageRows are the rows of the usage table; page names an app's or
// cache's target ("app:<id>") and links to its page, false for one gone.
func usageRows(rows []teldb.Sample, page func(target string) (name, link string, ok bool)) []usageRow {
	out := make([]usageRow, 0, len(rows))
	for _, r := range rows {
		row := usageRow{CPU: cores(r.Cpu), Memory: bytesText(float64(r.Mem)), MemPct: percentOf(float64(r.Mem), float64(r.MemLimit))}
		if r.MemLimit > 0 {
			row.Memory += fmt.Sprintf(" of %s (%d%%)", bytesText(float64(r.MemLimit)), row.MemPct)
		}
		kind, name, _ := strings.Cut(r.Target, ":")
		switch kind {
		case ops.HostTarget:
			row.Name = "server"
			row.CPU += fmt.Sprintf(" of %.0f", r.CpuLimit)
		case "app", "cache":
			var ok bool
			if row.Name, row.Link, ok = page(r.Target); !ok {
				continue
			}
		case "worker":
			var ok bool
			if row.Name, row.Link, ok = page("app:" + name); !ok {
				continue
			}
			row.Name, row.Link = row.Name+" worker", row.Link+"/settings#worker"
		case "others":
			row.Name = "other users' apps"
		default:
			row.Name = name
		}
		out = append(out, row)
	}
	return out
}

// targetPage is the name and page of an app's or cache's target
// ("app:<id>", "cache:<id>"); ok is false for one deleted since.
func targetPage(s *store.Store, target string) (name, link string, ok bool) {
	kind, rest, _ := strings.Cut(target, ":")
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		return "", "", false
	}
	switch kind {
	case "app":
		a, err := s.GetAppByID(context.Background(), id)
		if err != nil {
			return "", "", false
		}
		return a.Name, appURL(a), true
	case "cache":
		c, err := s.GetCacheByID(context.Background(), id)
		if err != nil {
			return "", "", false
		}
		return c.Name, ops.CachePath(s, c), true
	}
	return "", "", false
}

// cardUsage is a target's usage on its card: now, and the last day as
// sparkline points.
type cardUsage struct {
	CPU, Mem, MemLimit float64 // now; Mem and MemLimit in bytes
	Now                bool    // a reading of the last minutes
	CPUSpark, MemSpark template.HTML
}

// sparkPoints is how many points a card's sparkline has: quarter hours
// of the last day.
const sparkPoints = 96

// loadCardUsage reads target's last day for its card; ok is false with no
// readings at all.
func loadCardUsage(s *store.Store, target string) (u cardUsage, ok bool) {
	rows, err := ops.UsageOf(s, target, 24*time.Hour)
	if err != nil || len(rows) == 0 {
		return u, false
	}
	last := rows[len(rows)-1]
	u.Now = time.Since(time.Unix(last.Ts, 0)) < 3*time.Minute
	u.CPU, u.Mem, u.MemLimit = last.Cpu, float64(last.Mem), float64(last.MemLimit)
	from := time.Now().Add(-24 * time.Hour).Unix()
	step := int64(24*time.Hour/time.Second) / sparkPoints
	cpu, mem := make([]float64, sparkPoints), make([]float64, sparkPoints)
	n := make([]int, sparkPoints)
	for _, r := range rows {
		i := int((r.Ts - from) / step)
		if i < 0 || i >= sparkPoints {
			continue
		}
		cpu[i] += r.Cpu
		mem[i] += float64(r.Mem)
		n[i]++
	}
	for i := range n {
		if n[i] > 0 {
			cpu[i] /= float64(n[i])
			mem[i] /= float64(n[i])
		} else {
			cpu[i], mem[i] = math.NaN(), math.NaN()
		}
	}
	u.CPUSpark = sparkline(cpu)
	u.MemSpark = sparkline(mem) // its trend: how near the limit it is shows in figures
	return u, true
}

// sparkline draws values (NaN where there are none) as a small area
// behind a card's figure. It spans their own range, so a trend shows, but
// never less than a tenth of the highest, so noise doesn't look like one.
func sparkline(values []float64) template.HTML {
	const w, h = 200, 40
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, v := range values {
		if !math.IsNaN(v) {
			lo, hi = min(lo, v), max(hi, v)
		}
	}
	if math.IsInf(hi, -1) {
		return ""
	}
	span := max(hi-lo, hi*0.1, 1e-9)
	mid := (lo + hi) / 2
	lo, hi = max(0, mid-span*0.65), mid+span*0.65
	x := func(i int) float64 { return float64(i) / float64(max(len(values)-1, 1)) * w }
	y := func(v float64) float64 { return h - (v-lo)/(hi-lo)*h }
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="node-spark" viewBox="0 0 %d %d" preserveAspectRatio="none" aria-hidden="true">`, w, h)
	var run []int
	flush := func() {
		if len(run) > 1 {
			var line strings.Builder
			for _, i := range run {
				fmt.Fprintf(&line, "%.1f,%.1f ", x(i), y(values[i]))
			}
			pts := strings.TrimSpace(line.String())
			fmt.Fprintf(&b, `<polygon points="%.1f,%d %s %.1f,%d"/>`, x(run[0]), h, pts, x(run[len(run)-1]), h)
			fmt.Fprintf(&b, `<polyline points="%s" vector-effect="non-scaling-stroke"/>`, pts)
		}
		run = nil
	}
	for i, v := range values {
		if math.IsNaN(v) {
			flush()
			continue
		}
		run = append(run, i)
	}
	flush()
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// cpuPercent is CPU use as a share of one CPU.
func cpuPercent(v float64) string {
	if v > 0 && v < 0.01 {
		return "<1%"
	}
	return fmt.Sprintf("%.0f%%", v*100)
}

// memText is memory use, with its limit when there is one.
func memText(u cardUsage) string {
	used := bytesText(u.Mem)
	if u.MemLimit <= 0 {
		return used
	}
	limit := bytesText(u.MemLimit)
	if num, unit, _ := strings.Cut(used, " "); strings.HasSuffix(limit, " "+unit) {
		used = num // "182 / 256 MB"
	}
	return used + " / " + limit
}
