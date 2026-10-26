package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/hakobudev/hakobu/internal/alert"
	"github.com/hakobudev/hakobu/internal/registry"
	"github.com/hakobudev/hakobu/internal/s3"
)

// Settings are the parts of the config that change after the install:
// the projects' release images, registries, the backups' buckets, mail,
// the pulse and hakobu's updates. The API sets them (hakobu server set,
// hakobu init) and hakobu keeps them in settings.toml in its data
// directory, so no one edits the server's files; server.toml wins on
// whatever it sets itself.
type Settings struct {
	// Rev counts the changes, so the CLI sees when one is in effect.
	Rev        int                    `toml:"rev"`
	Releases   []string               `toml:"releases,omitempty"`
	Registries map[string]Credentials `toml:"registries,omitempty"`
	Backups    *Backups               `toml:"backups,omitempty"`
	Updates    *Updates               `toml:"updates,omitempty"`
	SMTP       *alert.SMTP            `toml:"smtp,omitempty"`
	Pulse      string                 `toml:"pulse_url,omitempty"`
	PulseFail  string                 `toml:"pulse_fail_url,omitempty"`
}

// LoadSettings reads settings.toml; none yet is no settings.
func LoadSettings(file string) (*Settings, error) {
	var s Settings
	md, err := toml.DecodeFile(file, &s)
	if errors.Is(err, os.ErrNotExist) {
		return &s, nil
	}
	if err != nil {
		return nil, err
	}
	if keys := md.Undecoded(); len(keys) > 0 {
		return nil, fmt.Errorf("unknown keys: %v", keys)
	}
	return &s, nil
}

// Save writes the settings readable by hakobu alone: they hold
// credentials, as server.toml does.
func (s *Settings) Save(file string) error {
	var b bytes.Buffer
	b.WriteString("# hakobu's settings, set through its API (hakobu server set): change them so, not here.\n")
	if err := toml.NewEncoder(&b).Encode(s); err != nil {
		return err
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, b.Bytes(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

// fixedKeys are the settings server.toml sets.
func fixedKeys(md toml.MetaData, c *Config) map[string]bool {
	f := map[string]bool{}
	for _, r := range c.Releases {
		f["releases:"+r] = true
	}
	for host := range c.Registries {
		f["registries."+host] = true
	}
	for _, k := range [][]string{{"backups"}, {"updates"}, {"alerts", "smtp"}, {"alerts", "pulse_url"}, {"alerts", "pulse_fail_url"}} {
		if md.IsDefined(k...) {
			f[strings.Join(k, ".")] = true
		}
	}
	return f
}

// merge puts in the settings server.toml doesn't set.
func (c *Config) merge(s *Settings) {
	for _, r := range s.Releases {
		if !slices.Contains(c.Releases, r) {
			c.Releases = append(c.Releases, r)
		}
	}
	for host, cr := range s.Registries {
		if c.Registries == nil {
			c.Registries = map[string]Credentials{}
		}
		if _, ok := c.Registries[host]; !ok {
			c.Registries[host] = cr
		}
	}
	if s.Backups != nil && !c.fixed["backups"] {
		c.Backups = *s.Backups
		c.Backups.Copies = slices.Clone(s.Backups.Copies)
		if p := s.Backups.Primary; p != nil {
			pp := *p
			c.Backups.Primary = &pp
		}
	}
	if s.Updates != nil && !c.fixed["updates"] {
		c.Updates = *s.Updates
	}
	if s.SMTP != nil && !c.fixed["alerts.smtp"] {
		m := *s.SMTP
		c.Alerts.SMTP = &m
	}
	if s.Pulse != "" && !c.fixed["alerts.pulse_url"] {
		c.Alerts.Pulse = s.Pulse
	}
	if s.PulseFail != "" && !c.fixed["alerts.pulse_fail_url"] {
		c.Alerts.PulseFail = s.PulseFail
	}
}

// SettingsChange is one change of the settings: each part that's there is
// set (a release added or removed, a registry, a bucket); Remove* drop one.
type SettingsChange struct {
	AddRelease     string       `json:"add_release,omitempty"`
	RemoveRelease  string       `json:"remove_release,omitempty"`
	Registry       *RegistrySet `json:"registry,omitempty"`
	SMTP           *alert.SMTP  `json:"smtp,omitempty"`
	RemoveSMTP     bool         `json:"remove_smtp,omitempty"`
	Pulse          *PulseSet    `json:"pulse,omitempty"`
	Updates        *Updates     `json:"updates,omitempty"`
	Backups        *BackupsSet  `json:"backups,omitempty"`
	RemoveBackups  bool         `json:"remove_backups,omitempty"`
	RemoveCopyName string       `json:"remove_copy,omitempty"`
}

type RegistrySet struct {
	Host     string `json:"host"`
	Username string `json:"username"`
	Password string `json:"password"`
	Remove   bool   `json:"remove,omitempty"`
}

type PulseSet struct {
	URL     string `json:"url"`
	FailURL string `json:"fail_url"`
}

// BackupsSet sets the primary bucket, or one copy (Copy true, by its
// name), and when and how many backups are kept (0, "": as they are).
type BackupsSet struct {
	Bucket *Bucket `json:"bucket,omitempty"`
	Copy   bool    `json:"copy,omitempty"`
	At     string  `json:"at,omitempty"`
	Daily  int     `json:"keep_daily,omitempty"`
	Weekly int     `json:"keep_weekly,omitempty"`
}

// admin tells whether the change needs the admin token: one of the
// backups' buckets, which could stop the backups, or send them elsewhere.
func (ch SettingsChange) admin() bool {
	return ch.Backups != nil || ch.RemoveBackups || ch.RemoveCopyName != ""
}

// apply is s with the change, or why it can't be: a key server.toml sets
// stays as server.toml has it.
func (ch SettingsChange) apply(s Settings, c *Config) (Settings, error) {
	conflict := func(key string) error {
		return apiError{http.StatusConflict, fmt.Sprintf("%s is set in %s on the server, which wins: change it there", key, ConfigFile)}
	}
	n := s
	n.Releases = slices.Clone(s.Releases)
	if r := ch.AddRelease; r != "" {
		if !slices.Contains(n.Releases, r) && !c.fixed["releases:"+r] {
			n.Releases = append(n.Releases, r)
		}
	}
	if r := ch.RemoveRelease; r != "" {
		if c.fixed["releases:"+r] {
			return s, conflict("releases " + r)
		}
		if !slices.Contains(n.Releases, r) {
			return s, badRequest("no release %s here", r)
		}
		n.Releases = slices.DeleteFunc(n.Releases, func(x string) bool { return x == r })
	}
	if rg := ch.Registry; rg != nil {
		if rg.Host == "" {
			return s, badRequest("registry: which host?")
		}
		if c.fixed["registries."+rg.Host] {
			return s, conflict("registries." + rg.Host)
		}
		n.Registries = map[string]Credentials{}
		for k, v := range s.Registries {
			n.Registries[k] = v
		}
		if rg.Remove {
			delete(n.Registries, rg.Host)
		} else {
			n.Registries[rg.Host] = Credentials{Username: rg.Username, Password: rg.Password}
		}
	}
	if ch.SMTP != nil || ch.RemoveSMTP {
		if c.fixed["alerts.smtp"] {
			return s, conflict("alerts.smtp")
		}
		n.SMTP = ch.SMTP
	}
	if p := ch.Pulse; p != nil {
		if (p.URL != "" && c.fixed["alerts.pulse_url"]) || (p.FailURL != "" && c.fixed["alerts.pulse_fail_url"]) {
			return s, conflict("alerts.pulse_url")
		}
		n.Pulse, n.PulseFail = p.URL, p.FailURL
	}
	if u := ch.Updates; u != nil {
		if c.fixed["updates"] {
			return s, conflict("updates")
		}
		uu := *u
		n.Updates = &uu
	}
	if ch.admin() {
		if c.fixed["backups"] {
			return s, conflict("backups")
		}
		var b Backups
		if s.Backups != nil {
			b = *s.Backups
			b.Copies = slices.Clone(s.Backups.Copies)
		}
		if bs := ch.Backups; bs != nil {
			if bs.At != "" {
				b.At = bs.At
			}
			if bs.Daily != 0 {
				b.Daily = bs.Daily
			}
			if bs.Weekly != 0 {
				b.Weekly = bs.Weekly
			}
			if bk := bs.Bucket; bk != nil {
				nb := *bk
				if bs.Copy {
					if nb.Name == "" {
						return s, badRequest("a copy needs a name")
					}
					b.Copies = slices.DeleteFunc(b.Copies, func(x Bucket) bool { return x.Name == nb.Name })
					b.Copies = append(b.Copies, nb)
				} else {
					b.Primary = &nb
				}
			}
		}
		if name := ch.RemoveCopyName; name != "" {
			if !slices.ContainsFunc(b.Copies, func(x Bucket) bool { return x.Name == name }) {
				return s, badRequest("no copy %s", name)
			}
			b.Copies = slices.DeleteFunc(b.Copies, func(x Bucket) bool { return x.Name == name })
		}
		n.Backups = &b
		if ch.RemoveBackups {
			n.Backups = nil
		}
	}
	return n, nil
}

// check proves a change before it's kept: a mail server sends a mail, a
// bucket takes an object (and the primary gives it back).
func (ch SettingsChange) check(ctx context.Context, server string) error {
	if m := ch.SMTP; m != nil {
		if err := m.Send("hakobu "+server+": mail works", "hakobu on "+server+" will mail you here when something's wrong, and when it's over."); err != nil {
			return badRequest("the mail server: %v", err)
		}
	}
	if bs := ch.Backups; bs != nil && bs.Bucket != nil {
		b := bs.Bucket
		st := s3.New(b.Endpoint, b.Region, b.Bucket, b.AccessKey, b.SecretKey)
		key := b.Prefix + ".hakobu-check"
		body := []byte("hakobu " + server + " " + time.Now().UTC().Format(time.RFC3339))
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := st.Put(cctx, key, body); err != nil {
			return badRequest("bucket %s: writing: %v", b.Bucket, err)
		}
		if bs.Copy {
			return nil // a copy's key may only write
		}
		rc, err := st.Get(cctx, key)
		if err != nil {
			return badRequest("bucket %s: reading back: %v", b.Bucket, err)
		}
		got, err := io.ReadAll(io.LimitReader(rc, 1<<10))
		rc.Close()
		if err != nil || !bytes.Equal(got, body) {
			return badRequest("bucket %s: what it gave back isn't what was written", b.Bucket)
		}
		_ = st.Delete(cctx, key) // a locked bucket may keep it
	}
	return nil
}

// The API's side: GET shows the settings (never a password or key), POST
// changes them. A change is checked, kept, and in effect once hakobu has
// restarted itself, as soon as no deploy or backup is running.

// ErrRestart is Run's answer when the settings changed: run again.
var ErrRestart = errors.New("hakobu restarts for its new settings")

// SettingsView is the settings as the API shows them; Source says where
// each comes from, "server.toml" or "api".
type SettingsView struct {
	Rev        int            `json:"rev"`
	Releases   []ReleaseView  `json:"releases"`
	Registries []RegistryView `json:"registries"`
	Backups    *BackupsView   `json:"backups,omitempty"`
	Updates    UpdatesView    `json:"updates"`
	SMTP       *SMTPView      `json:"smtp,omitempty"`
	Pulse      string         `json:"pulse_url,omitempty"`
	PulseFail  string         `json:"pulse_fail_url,omitempty"`
}

type ReleaseView struct {
	Image  string `json:"image"`
	Source string `json:"source"`
}

type RegistryView struct {
	Host     string `json:"host"`
	Username string `json:"username"`
	Source   string `json:"source"`
}

type BackupsView struct {
	At      string       `json:"at"`
	Daily   int          `json:"keep_daily"`
	Weekly  int          `json:"keep_weekly"`
	Primary *BucketView  `json:"primary,omitempty"`
	Copies  []BucketView `json:"copies,omitempty"`
	Source  string       `json:"source"`
}

type BucketView struct {
	Name      string `json:"name,omitempty"`
	Endpoint  string `json:"endpoint"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix"`
	AccessKey string `json:"access_key"`
}

type UpdatesView struct {
	Auto   string `json:"auto"`
	At     string `json:"at"`
	Source string `json:"source"`
}

type SMTPView struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username,omitempty"`
	From     string `json:"from"`
	To       string `json:"to"`
	Source   string `json:"source"`
}

func source(fixed bool) string {
	if fixed {
		return "server.toml"
	}
	return "api"
}

func bucketView(b Bucket) BucketView {
	return BucketView{Name: b.Name, Endpoint: b.Endpoint, Bucket: b.Bucket, Prefix: b.Prefix, AccessKey: b.AccessKey}
}

func (d *Daemon) settings(w http.ResponseWriter, r *http.Request) (any, error) {
	c := d.Config
	rev := 0
	if c.Settings != nil {
		rev = c.Settings.Rev
	}
	v := SettingsView{Rev: rev, Pulse: c.Alerts.Pulse, PulseFail: c.Alerts.PulseFail,
		Updates: UpdatesView{Auto: c.Updates.Auto, At: c.Updates.At, Source: source(c.fixed["updates"])}}
	for _, rel := range c.Releases {
		v.Releases = append(v.Releases, ReleaseView{Image: rel, Source: source(c.fixed["releases:"+rel])})
	}
	for _, host := range sortedHosts(c.Registries) {
		v.Registries = append(v.Registries, RegistryView{Host: host, Username: c.Registries[host].Username, Source: source(c.fixed["registries."+host])})
	}
	if c.Backups.Primary != nil {
		b := &BackupsView{At: c.Backups.At, Daily: c.Backups.Daily, Weekly: c.Backups.Weekly, Source: source(c.fixed["backups"])}
		pv := bucketView(*c.Backups.Primary)
		b.Primary = &pv
		for _, cp := range c.Backups.Copies {
			b.Copies = append(b.Copies, bucketView(cp))
		}
		v.Backups = b
	}
	if m := c.Alerts.SMTP; m != nil {
		v.SMTP = &SMTPView{Host: m.Host, Port: m.Port, Username: m.Username, From: m.From, To: m.To, Source: source(c.fixed["alerts.smtp"])}
	}
	return v, nil
}

func sortedHosts(m map[string]Credentials) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func (d *Daemon) changeSettings(w http.ResponseWriter, r *http.Request) (any, error) {
	var ch SettingsChange
	if err := decode(r, &ch); err != nil {
		return nil, err
	}
	if ch.admin() {
		if d.levelOf(r) != Admin {
			return nil, apiError{http.StatusForbidden, "the backups' buckets need the admin token"}
		}
	}
	// A release image that deployed a project is the only one that may:
	// removing it lets another image take the project, and its databases.
	var freed *registry.Ref
	if rel := ch.RemoveRelease; rel != "" {
		if ref, err := registry.ParseRef(rel); err == nil {
			project, err := d.Watch.Claimed(r.Context(), ref)
			if err != nil {
				return nil, err
			}
			if project != "" {
				if d.levelOf(r) != Admin {
					return nil, apiError{http.StatusForbidden, fmt.Sprintf("%s deploys project %s: removing it lets another release image deploy %s, so it needs the admin token (ssh root@… hakobu server set release --remove %s)", rel, project, project, rel)}
				}
				freed = &ref
			}
		}
	}
	if bs := ch.Backups; bs != nil && bs.Bucket != nil && bs.Bucket.Prefix == "" {
		bs.Bucket.Prefix = "hakobu/" + d.Config.Name + "/"
	}
	d.settingsMu.Lock()
	defer d.settingsMu.Unlock()
	// The file, not the settings in effect: changes may come faster than
	// restarts.
	cur, err := LoadSettings(d.Config.SettingsFile())
	if err != nil {
		return nil, err
	}
	n, err := ch.apply(*cur, d.Config)
	if err != nil {
		return nil, err
	}
	n.Rev = cur.Rev + 1
	if _, err := ParseConfigWith(d.Config.base, &n); err != nil {
		return nil, badRequest("%v", err)
	}
	if err := ch.check(r.Context(), d.Config.Name); err != nil {
		return nil, err
	}
	if err := n.Save(d.Config.SettingsFile()); err != nil {
		return nil, err
	}
	if freed != nil {
		if err := d.Watch.Forget(*freed); err != nil {
			fmt.Fprintln(d.log(), "warning:", err)
		}
	}
	select {
	case d.restart <- struct{}{}:
	default: // one is asked for already
	}
	return map[string]int{"rev": n.Rev}, nil
}

// restartWhenAsked ends Run (stop) once a restart is asked for and no
// deploy or backup is running; serve then runs hakobu again.
func (d *Daemon) restartWhenAsked(ctx context.Context, stop func()) {
	select {
	case <-ctx.Done():
		return
	case <-d.restart:
	}
	for {
		d.mu.Lock()
		busy := d.backingUp
		d.mu.Unlock()
		if !busy && !d.Applier.Busy() {
			d.mu.Lock()
			d.restarting = true
			d.mu.Unlock()
			fmt.Fprintln(d.log(), "restarting for the new settings")
			stop()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}
