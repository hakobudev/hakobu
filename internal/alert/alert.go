// Package alert emails the server's owner when something needs them: once
// when a problem starts, once when it's over, never again in between, even
// across restarts.
package alert

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Mailer sends an email to the owner.
type Mailer interface {
	Send(subject, body string) error
}

// SMTP sends through any mail server: port 465 speaks TLS from the start,
// others (587) upgrade with STARTTLS, which net/smtp insists on before it
// sends a password.
type SMTP struct {
	Host     string `toml:"host"`
	Port     int    `toml:"port"`
	Username string `toml:"username"`
	Password string `toml:"password"`
	From     string `toml:"from"`
	To       string `toml:"to"`
}

// sendTimeout bounds a whole mail, from the connection to QUIT: a mail
// server that hangs mustn't hold the alerts, nor the deploys that raise them.
var sendTimeout = time.Minute

func (s SMTP) Send(subject, body string) error {
	addr := net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	msg := strings.Join([]string{
		"From: " + s.From,
		"To: " + s.To,
		"Subject: " + strings.ReplaceAll(subject, "\n", " "),
		"Date: " + time.Now().Format(time.RFC1123Z),
		"Content-Type: text/plain; charset=utf-8",
		"", body,
	}, "\r\n")
	dialer := &net.Dialer{Timeout: sendTimeout}
	var conn net.Conn
	var err error
	if s.Port == 465 {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(sendTimeout)); err != nil {
		return err
	}
	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		return err
	}
	defer c.Close()
	if s.Port != 465 {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(&tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12}); err != nil {
				return err
			}
		}
	}
	if s.Username != "" {
		// PlainAuth refuses to send the password unencrypted.
		if err := c.Auth(smtp.PlainAuth("", s.Username, s.Password, s.Host)); err != nil {
			return err
		}
	}
	if err := c.Mail(s.From); err != nil {
		return err
	}
	if err := c.Rcpt(s.To); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// Alerts keeps which problems are open, in File, so each is mailed once.
type Alerts struct {
	Mailer Mailer
	Server string // in the subjects
	File   string
	Now    func() time.Time

	mu sync.Mutex
}

// Problem is an open alert.
type Problem struct {
	Since   time.Time `json:"since"`
	Subject string    `json:"subject"`
	Body    string    `json:"body"`
	// Mailed tells whether the mail went: one that failed is tried again.
	Mailed bool `json:"mailed"`
}

// Raise opens the problem key, mailing it if it wasn't open (or its mail
// never went). An open one is left as it is.
func (a *Alerts) Raise(key, subject, body string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	open, err := a.load()
	if err != nil {
		return err
	}
	p, ok := open[key]
	if ok && p.Mailed {
		return nil
	}
	if !ok {
		p = Problem{Since: a.now(), Subject: subject, Body: body}
	}
	err = a.mail("[hakobu "+a.Server+"] "+subject, body)
	p.Mailed = err == nil
	open[key] = p
	if serr := a.save(open); serr != nil {
		return errors.Join(err, serr)
	}
	return err
}

// Clear closes the problem key, mailing that it's over if it was mailed.
func (a *Alerts) Clear(key, body string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	open, err := a.load()
	if err != nil {
		return err
	}
	p, ok := open[key]
	if !ok {
		return nil
	}
	delete(open, key)
	if err := a.save(open); err != nil {
		return err
	}
	if !p.Mailed {
		return nil
	}
	if body == "" {
		body = "It's over: " + p.Subject
	}
	return a.mail(fmt.Sprintf("[hakobu %s] resolved: %s (after %s)", a.Server, p.Subject, a.now().Sub(p.Since).Round(time.Minute)), body)
}

// Set raises key with the subject when bad, and clears it otherwise.
func (a *Alerts) Set(key string, bad bool, subject, body string) error {
	if bad {
		return a.Raise(key, subject, body)
	}
	return a.Clear(key, "")
}

// Open are the open problems, oldest first.
func (a *Alerts) Open() ([]Problem, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	open, err := a.load()
	if err != nil {
		return nil, err
	}
	var out []Problem
	for _, p := range open {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out, nil
}

// Keys are the open problems' keys that start with prefix.
func (a *Alerts) Keys(prefix string) ([]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	open, err := a.load()
	if err != nil {
		return nil, err
	}
	var out []string
	for k := range open {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (a *Alerts) mail(subject, body string) error {
	if a.Mailer == nil {
		return errors.New("no mail server configured")
	}
	return a.Mailer.Send(subject, body)
}

func (a *Alerts) load() (map[string]Problem, error) {
	open := map[string]Problem{}
	b, err := os.ReadFile(a.File)
	if errors.Is(err, os.ErrNotExist) {
		return open, nil
	}
	if err != nil {
		return nil, err
	}
	return open, json.Unmarshal(b, &open)
}

func (a *Alerts) save(open map[string]Problem) error {
	b, err := json.MarshalIndent(open, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.File + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.File)
}

func (a *Alerts) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}
