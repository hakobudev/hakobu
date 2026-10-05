// Package panellog is the panel's own log: what hakobu itself does and
// what goes wrong for it, as opposed to its apps' logs. Each line goes to
// standard output, which systemd keeps in the journal with its level
// (journalctl -u hakobu -p err shows the errors alone), and to the sink,
// which keeps it for Settings → Panel log.
package panellog

import (
	"fmt"
	"os"
	"strings"
	"sync/atomic"
)

type Level int

const (
	LevelInfo Level = iota
	LevelWarn
	LevelError
)

func (l Level) String() string {
	return [...]string{"info", "warn", "error"}[l]
}

// priority is the line's syslog priority, which systemd reads from a
// "<n>" at its start.
func (l Level) priority() int {
	return [...]int{6, 4, 3}[l]
}

var (
	sink atomic.Pointer[func(Level, string)]
	// Under systemd, stdout goes to the journal, which takes the "<n>"
	// prefix for the level; a terminal would show it.
	journal = os.Getenv("JOURNAL_STREAM") != ""
)

// SetSink has every line from now on also go to f, which mustn't log
// through this package itself.
func SetSink(f func(Level, string)) {
	if f == nil {
		sink.Store(nil)
		return
	}
	sink.Store(&f)
}

func emit(l Level, msg string) {
	msg = strings.TrimRight(msg, "\n")
	if journal {
		fmt.Printf("<%d>%s\n", l.priority(), msg)
	} else {
		fmt.Println(msg)
	}
	if f := sink.Load(); f != nil {
		(*f)(l, msg)
	}
}

// Info, Warn and Error log their operands as fmt.Println would join them.
func Info(a ...any)  { emit(LevelInfo, fmt.Sprintln(a...)) }
func Warn(a ...any)  { emit(LevelWarn, fmt.Sprintln(a...)) }
func Error(a ...any) { emit(LevelError, fmt.Sprintln(a...)) }

// Infof, Warnf and Errorf format as fmt.Printf would.
func Infof(format string, a ...any)  { emit(LevelInfo, fmt.Sprintf(format, a...)) }
func Warnf(format string, a ...any)  { emit(LevelWarn, fmt.Sprintf(format, a...)) }
func Errorf(format string, a ...any) { emit(LevelError, fmt.Sprintf(format, a...)) }
