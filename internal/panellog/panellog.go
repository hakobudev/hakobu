// Package panellog is hakobu's own log: what it does and what goes wrong
// for it, as opposed to its apps' logs. Each line goes to standard output,
// which systemd keeps in the journal with its level (journalctl -u hakobu
// -p err shows the errors alone).
package panellog

import (
	"fmt"
	"os"
	"strings"
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

// Under systemd, stdout goes to the journal, which takes the "<n>" prefix
// for the level; a terminal would show it.
var journal = os.Getenv("JOURNAL_STREAM") != ""

func emit(l Level, msg string) {
	msg = strings.TrimRight(msg, "\n")
	if journal {
		fmt.Printf("<%d>%s\n", l.priority(), msg)
	} else {
		fmt.Println(msg)
	}
}

// Info, Warn and Error log their operands as fmt.Println would join them.
func Info(a ...any)  { emit(LevelInfo, fmt.Sprintln(a...)) }
func Warn(a ...any)  { emit(LevelWarn, fmt.Sprintln(a...)) }
func Error(a ...any) { emit(LevelError, fmt.Sprintln(a...)) }
