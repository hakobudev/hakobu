package cmd

import (
	"context"
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
	Title string
	Now   string // the latest value, with the limit if any
	Peak  string // the highest over the span
	SVG   template.HTML
}

// usageChart draws the series over [from, to]; ts are the samples' times
// and res their spacing, so missing samples leave a gap.
func usageChart(title string, ts []int64, res, from, to int64, limit float64, format func(float64) string, lines ...series) chart {
	c := chart{Title: title}
	if len(ts) == 0 {
		return c
	}
	top := limit
	for _, l := range lines {
		for _, v := range append(slices.Clone(l.avg), l.peak...) {
			top = max(top, v)
		}
	}
	if top <= 0 {
		top = 1
	}
	top *= 1.1
	x := func(t int64) float64 { return float64(t-from) / float64(max(to-from, 1)) * chartW }
	y := func(v float64) float64 { return chartH - v/top*chartH }

	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %d %d" preserveAspectRatio="none" style="display:block;width:100%%;height:7rem" role="img" aria-label="%s">`, chartW, chartH, template.HTMLEscapeString(title))
	for _, frac := range []float64{0.25, 0.5, 0.75} {
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
	if limit > 0 {
		fmt.Fprintf(&b, `<line x1="0" x2="%d" y1="%.1f" y2="%.1f" style="stroke:var(--danger)" stroke-dasharray="4 3" stroke-width="1" vector-effect="non-scaling-stroke"/>`, chartW, y(limit), y(limit))
	}
	b.WriteString(`</svg>`)
	c.SVG = template.HTML(b.String())

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
		usageChart(title+" · CPU", ts, res, from, to, last.CpuLimit, cores, series{name: "CPU", color: "var(--chart-1)", avg: cpu, peak: cpuMax}),
		usageChart(title+" · memory", ts, res, from, to, float64(last.MemLimit), bytesText, series{name: "memory", color: "var(--chart-2)", avg: mem, peak: memMax}),
	}
	if target == ops.HostTarget {
		return append(charts, usageChart(title+" · load", ts, res, from, to, 0, loadText, series{name: "load", color: "var(--chart-3)", avg: load}))
	}
	return append(charts, usageChart(title+" · network", ts, res, from, to, 0, rateText,
		series{name: "in", color: "var(--chart-4)", avg: rx}, series{name: "out", color: "var(--chart-3)", avg: tx}))
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

// sparkPoints is how many points a card's sparkline has: half hours of
// the last day.
const sparkPoints = 48

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
	u.CPUSpark = sparkline(cpu, 0)
	u.MemSpark = sparkline(mem, 0) // its trend: how near the limit it is shows in figures
	return u, true
}

// sparkline draws values (NaN where there are none) as a small line,
// scaled to top if it's higher than them all.
func sparkline(values []float64, top float64) template.HTML {
	const w, h = 100, 20
	for _, v := range values {
		if !math.IsNaN(v) {
			top = max(top, v)
		}
	}
	if top <= 0 {
		top = 1
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="spark" viewBox="0 0 %d %d" preserveAspectRatio="none" aria-hidden="true">`, w, h)
	var pts []string
	flush := func() {
		if len(pts) > 1 {
			fmt.Fprintf(&b, `<polyline points="%s" vector-effect="non-scaling-stroke"/>`, strings.Join(pts, " "))
		}
		pts = nil
	}
	for i, v := range values {
		if math.IsNaN(v) {
			flush()
			continue
		}
		x := float64(i) / float64(max(len(values)-1, 1)) * w
		y := h - 1 - v/top*(h-2)
		pts = append(pts, fmt.Sprintf("%.1f,%.1f", x, y))
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
