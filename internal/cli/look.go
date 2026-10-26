package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/term"
)

// How the commands' output looks: a symbol and a word say what's wrong or
// fine, so nothing leans on colour (nor on red against green); colour and
// bold only help, at a terminal; NO_COLOR turns them off.
const (
	symOK     = "●" // runs, went through
	symBad    = "✕" // down, failed
	symData   = "◆" // a database, cache or volume
	symBack   = "↺" // rolled back
	symUpdate = "↑" // a newer hakobu
)

type look struct{ on bool }

// lookFor styles what goes to w if w is a terminal that takes colour, or
// CLICOLOR_FORCE asks for it anyway (ssh without -t has no terminal).
func lookFor(w io.Writer) look {
	if os.Getenv("NO_COLOR") != "" {
		return look{}
	}
	if f := os.Getenv("CLICOLOR_FORCE"); f != "" && f != "0" {
		return look{true}
	}
	f, ok := w.(*os.File)
	return look{ok && term.IsTerminal(int(f.Fd())) && os.Getenv("TERM") != "dumb"}
}

func (l look) paint(code, s string) string {
	if !l.on || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (l look) bold(s string) string { return l.paint("1", s) }
func (l look) dim(s string) string  { return l.paint("90", s) }
func (l look) bad(s string) string  { return l.paint("1;91", s) }
func (l look) ok(s string) string   { return l.paint("94", s) }
func (l look) warn(s string) string { return l.paint("33", s) }

// pad fills s, as it shows, to n columns; styled is what's printed.
func pad(styled, plain string, n int) string {
	if w := utf8.RuneCountInString(plain); w < n {
		return styled + strings.Repeat(" ", n-w)
	}
	return styled
}

// width is the widest of the strings, as they show.
func width(ss ...string) int {
	n := 0
	for _, s := range ss {
		n = max(n, utf8.RuneCountInString(s))
	}
	return n
}

// meter is a thin bar of share (0–1), heavy for the used part and light
// for the rest, so it reads without colour too.
func (l look) meter(share float64, warn bool) string {
	const cells = 20
	n := min(cells, max(0, int(share*cells+0.5)))
	used := strings.Repeat("━", n)
	if warn {
		used = l.warn(used)
	} else {
		used = l.ok(used)
	}
	return used + l.dim(strings.Repeat("─", cells-n))
}

// since is how long ago t was, short: "40 min", "6 h", "3 days".
func since(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}

// split is a problem's or error's subject and its detail, at the first
// ": " ("crm/web is down", "GET /up: 502").
func split(s string) (string, string) {
	head, rest, _ := strings.Cut(firstLine(printable(s)), ": ")
	return head, rest
}
