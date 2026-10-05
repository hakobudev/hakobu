package panellog

import "testing"

func TestSink(t *testing.T) {
	var got []string
	SetSink(func(l Level, msg string) { got = append(got, l.String()+" "+msg) })
	t.Cleanup(func() { SetSink(nil) })
	Info("server", "x", "connected")
	Warnf("routes not updated (retrying in %s)\n", "5s")
	Error("failed:", "boom")
	if want := []string{"info server x connected", "warn routes not updated (retrying in 5s)", "error failed: boom"}; len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("sink got %q", got)
	}
	SetSink(nil)
	Info("no sink, no panic")
}
