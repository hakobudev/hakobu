package alert

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type inbox struct {
	sent []string
	fail error
}

func (i *inbox) Send(subject, body string) error {
	if i.fail != nil {
		return i.fail
	}
	i.sent = append(i.sent, subject)
	return nil
}

func TestOncePerProblem(t *testing.T) {
	var box inbox
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	file := filepath.Join(t.TempDir(), "alerts.json")
	a := &Alerts{Mailer: &box, Server: "kasl-1", File: file, Now: func() time.Time { return now }}
	for range 3 {
		if err := a.Raise("disk", "disk 91% full", "…"); err != nil {
			t.Fatal(err)
		}
	}
	// A restart reads the open problems back: no repeat either.
	a = &Alerts{Mailer: &box, Server: "kasl-1", File: file, Now: func() time.Time { return now }}
	_ = a.Raise("disk", "disk 91% full", "…")
	if len(box.sent) != 1 || box.sent[0] != "[hakobu kasl-1] disk 91% full" {
		t.Errorf("sent %q", box.sent)
	}
	now = now.Add(90 * time.Minute)
	if err := a.Clear("disk", ""); err != nil {
		t.Fatal(err)
	}
	_ = a.Clear("disk", "")
	if len(box.sent) != 2 || box.sent[1] != "[hakobu kasl-1] resolved: disk 91% full (after 1h30m0s)" {
		t.Errorf("sent %q", box.sent)
	}
	if open, _ := a.Open(); len(open) != 0 {
		t.Errorf("open %v", open)
	}
}

func TestFailedMailIsTriedAgain(t *testing.T) {
	box := inbox{fail: errors.New("smtp down")}
	a := &Alerts{Mailer: &box, Server: "s", File: filepath.Join(t.TempDir(), "a.json")}
	if err := a.Raise("x", "x broke", ""); err == nil {
		t.Error("a failed mail wasn't reported")
	}
	box.fail = nil
	if err := a.Raise("x", "x broke", ""); err != nil || len(box.sent) != 1 {
		t.Errorf("retry: %v, %q", err, box.sent)
	}
	// A problem that never got mailed ends without a "resolved".
	box2 := inbox{fail: errors.New("down")}
	b := &Alerts{Mailer: &box2, Server: "s", File: filepath.Join(t.TempDir(), "b.json")}
	_ = b.Raise("y", "y", "")
	box2.fail = nil
	if err := b.Clear("y", ""); err != nil || len(box2.sent) != 0 {
		t.Errorf("clear: %v %q", err, box2.sent)
	}
}

// TestSMTP talks to a minimal mail server on port 25-style plain SMTP.
func TestSMTP(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	got := make(chan string, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		say := func(s string) { _, _ = fmt.Fprint(c, s+"\r\n") }
		say("220 test")
		var data strings.Builder
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			switch {
			case inData && line == ".\r\n":
				inData = false
				got <- data.String()
				say("250 ok")
			case inData:
				data.WriteString(line)
			case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
				say("250 test")
			case strings.HasPrefix(line, "DATA"):
				inData = true
				say("354 go")
			case strings.HasPrefix(line, "QUIT"):
				say("221 bye")
				return
			default:
				say("250 ok")
			}
		}
	}()
	host, port, _ := net.SplitHostPort(l.Addr().String())
	var p int
	if _, err := fmt.Sscan(port, &p); err != nil {
		t.Fatal(err)
	}
	s := SMTP{Host: host, Port: p, From: "hakobu@kasl.example", To: "owner@example.com"}
	if err := s.Send("disk 91% full", "free some space"); err != nil {
		t.Fatal(err)
	}
	msg := <-got
	for _, want := range []string{"Subject: disk 91% full", "To: owner@example.com", "free some space"} {
		if !strings.Contains(msg, want) {
			t.Errorf("%q missing from\n%s", want, msg)
		}
	}
}

// A mail server that answers nothing is given up on.
func TestSMTPHungServer(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err == nil {
			defer c.Close()
			time.Sleep(5 * time.Second) // never says hello
		}
	}()
	old := sendTimeout
	sendTimeout = 200 * time.Millisecond
	defer func() { sendTimeout = old }()
	host, port, _ := net.SplitHostPort(l.Addr().String())
	var p int
	_, _ = fmt.Sscan(port, &p)
	start := time.Now()
	if err := (SMTP{Host: host, Port: p, From: "a@b.c", To: "d@e.f"}).Send("s", "b"); err == nil {
		t.Error("a hung server took the mail")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("gave up after %s", d)
	}
}
