package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/hakobudev/hakobu/internal/apply"
	"github.com/hakobudev/hakobu/internal/backups"
	"github.com/hakobudev/hakobu/internal/server"
)

// printDeploys is a server's last deploys, newest first, one a line.
func printDeploys(w io.Writer, name string, rs []apply.Result) {
	l := lookFor(w)
	fmt.Fprintln(w, l.bold(name)+"  "+l.dim(fmt.Sprintf("last %d deploys", len(rs))))
	if len(rs) == 0 {
		fmt.Fprintln(w, l.dim("  none yet"))
		return
	}
	fmt.Fprintln(w)
	whats, whens, outcomes := make([]string, len(rs)), make([]string, len(rs)), make([]string, len(rs))
	for i, r := range rs {
		whats[i] = strings.TrimSpace(r.Project + " " + shortRev(r.Revision))
		whens[i] = since(r.At) + " ago"
		switch {
		case r.Error != "":
			outcomes[i] = "failed"
		case r.Rollback:
			outcomes[i] = "rolled back"
		case len(r.Changes) == 1:
			outcomes[i] = "1 change"
		default:
			outcomes[i] = fmt.Sprintf("%d changes", len(r.Changes))
		}
	}
	ww, nw, ow := width(whats...), width(whens...), width(outcomes...)
	for i, r := range rs {
		sym, outcome, detail := l.ok(symOK), outcomes[i], ""
		switch {
		case r.Error != "":
			sym, outcome = l.bad(symBad), l.bad(outcomes[i])
			_, detail = split(strings.TrimPrefix(r.Error, "rollback: "))
			if detail == "" {
				detail, _ = split(r.Error)
			}
		case r.Rollback:
			sym, outcome = l.warn(symBack), l.warn(outcomes[i])
		default:
			detail = strings.Join(changed(r.Changes), ", ")
		}
		project, rev, _ := strings.Cut(whats[i], " ")
		what := l.bold(project)
		if rev != "" {
			what += " " + rev
		}
		fmt.Fprintln(w, strings.TrimRight(fmt.Sprintf("  %s %s  %s  %s  %s", sym, pad(what, whats[i], ww), pad(l.dim(whens[i]), whens[i], nw), pad(outcome, outcomes[i], ow), l.dim(detail)), " "))
	}
}

// changed is what each change was to, from Change.String ("update app
// web  …"): web, admin, cache.
func changed(cs []string) []string {
	var out []string
	for _, c := range cs {
		if f := strings.Fields(c); len(f) >= 3 {
			out = append(out, printable(f[2]))
		}
	}
	return out
}

// printBackups is a project's backups, newest first.
func printBackups(w io.Writer, name, project string, ms []backups.Manifest) {
	l := lookFor(w)
	fmt.Fprintln(w, l.bold(project)+"  "+l.dim("backups on "+name+", newest first"))
	if len(ms) == 0 {
		fmt.Fprintln(w, l.dim("  none yet"))
		return
	}
	fmt.Fprintln(w)
	names, whens := make([]string, len(ms)), make([]string, len(ms))
	for i, m := range ms {
		names[i] = printable(m.Name)
		whens[i] = m.At.Local().Format("Jan 2 15:04")
	}
	nw, ww := width(names...), width(whens...)
	for i, m := range ms {
		fmt.Fprintf(w, "  %s %s  %s  %s  %s\n", l.ok(symData), pad(names[i], names[i], nw), pad(l.dim(whens[i]), whens[i], ww), printable(m.Checked), l.dim(sizeOf(m.Size)))
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, l.dim("restore: ssh -t root@"+name+" hakobu restore "+project+"/NAME"))
}

// printBackupRun is what a backup made, and what failed and why.
func printBackupRun(w io.Writer, name string, run server.BackupRun) {
	l := lookFor(w)
	fmt.Fprintln(w, l.bold(name)+"  "+l.dim("backed up now"))
	fmt.Fprintln(w)
	type line struct {
		ok           bool
		what, detail string
	}
	var lines []line
	for _, m := range run.Made {
		// "database crm/main of 2026-10-26 03:00 (1.2 MB, 21 tables)"
		_, rest, _ := strings.Cut(printable(m), " ")
		what, rest, _ := strings.Cut(rest, " of ")
		detail := ""
		if i := strings.LastIndex(rest, "("); i >= 0 {
			detail = strings.TrimSuffix(rest[i+1:], ")")
		}
		lines = append(lines, line{true, what, detail})
	}
	for _, e := range run.Errors {
		what, why := split(e)
		lines = append(lines, line{false, what, why})
	}
	whats := make([]string, len(lines))
	for i, x := range lines {
		whats[i] = x.what
	}
	ww := width(whats...)
	for _, x := range lines {
		if x.ok {
			fmt.Fprintf(w, "  %s %s  %s\n", l.ok(symData), pad(x.what, x.what, ww), "sealed, copied, read back: "+x.detail)
		} else {
			fmt.Fprintf(w, "  %s %s  %s  %s\n", l.bad(symBad), pad(x.what, x.what, ww), l.bad("failed"), l.dim(x.detail))
		}
	}
	if len(run.Errors) == 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, l.ok(fmt.Sprintf("%s %d backed up", symOK, len(run.Made))))
	}
}

func sizeOf(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f kB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// deployed is a deploy or rollback as the server sends it, with its log
// and steps.
type deployed struct {
	apply.Result
	Log   string       `json:"log"`
	Steps []apply.Step `json:"steps"`
}

// ErrShown is an error the command has already shown, in its output.
var ErrShown = errors.New("failed, as shown above")

// printDeploy is a deploy or rollback step by step, and how it ended.
func printDeploy(w io.Writer, name string, d deployed) {
	l := lookFor(w)
	head := l.bold(printable(d.Project))
	if rev := shortRev(d.Revision); rev != "" && d.Rollback {
		head += "  back to " + rev
	} else if rev != "" {
		head += "  " + rev
	}
	head += "  " + l.dim(name)
	if d.Error == "" && len(d.Changes) == 0 {
		fmt.Fprintln(w, head+"  "+l.ok(symOK+" nothing to change"))
		return
	}
	fmt.Fprintln(w, head)
	fmt.Fprintln(w)
	var took time.Duration
	failed := ""
	for _, s := range d.Steps {
		took += s.Took
		sym := l.ok(symOK)
		if s.Error != "" {
			sym, failed = l.bad(symBad), s.Name
		}
		line := "  " + sym + " " + pad(s.Name, s.Name, 8) + " " + printable(s.Detail)
		if s.Took >= time.Second {
			line += l.dim(" · " + seconds(s.Took))
		}
		fmt.Fprintln(w, line)
		if s.Error != "" {
			fmt.Fprintln(w, strings.Repeat(" ", 13)+l.dim(firstLine(printable(s.Error))))
		}
	}
	if len(d.Steps) > 0 {
		fmt.Fprintln(w)
	}
	switch {
	case d.Error != "" && (failed == "worker" || failed == "remove"):
		fmt.Fprintln(w, l.bad(symBad+" not "+outcome(d))+l.dim(" · the new version is live"))
	case d.Error != "" && failed != "":
		// Up to the switch, a failed step leaves the live version alone.
		fmt.Fprintln(w, l.bad(symBad+" not "+outcome(d))+l.dim(" · the live version keeps running"))
	case d.Error != "":
		fmt.Fprintln(w, l.bad(symBad+" not "+outcome(d))+"  "+l.dim(firstLine(printable(d.Error))))
	case d.Rollback:
		fmt.Fprintln(w, l.warn(symBack+" rolled back")+l.dim(" in "+seconds(took)+" · the release after it waits for a newer one"))
	default:
		fmt.Fprintln(w, l.ok(symOK+" deployed")+l.dim(" in "+seconds(took)))
	}
}

func outcome(d deployed) string {
	if d.Rollback {
		return "rolled back"
	}
	return "deployed"
}

// seconds is a step's time, short: "0.4 s", "38 s", "2 min 5 s".
func seconds(d time.Duration) string {
	switch {
	case d < 10*time.Second:
		return fmt.Sprintf("%.1f s", d.Seconds())
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	}
	return fmt.Sprintf("%d min %d s", int(d.Minutes()), int(d.Seconds())%60)
}
