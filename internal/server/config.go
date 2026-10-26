// Package server is hakobu running on a server: it reads the server's
// config, deploys its projects' releases as they come, backs them up
// daily and answers the CLI on the tailnet.
package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/BurntSushi/toml"
	"github.com/hakobudev/hakobu/internal/alert"
	"github.com/hakobudev/hakobu/internal/ingress"
	"github.com/hakobudev/hakobu/internal/registry"
	"github.com/hakobudev/hakobu/internal/update"
)

// ConfigFile is the server's config: its name, where it listens, its
// projects' release images and its buckets. It holds credentials, so only
// hakobu reads it.
const ConfigFile = "/etc/hakobu/server.toml"

type Config struct {
	Name string `toml:"name"`
	// Listen is the API's address, on the tailnet or loopback (reached
	// through SSH): never a public one.
	Listen string `toml:"listen"`
	// DataDir holds the key, the state and the history.
	DataDir string `toml:"data_dir"`
	// OwnerKey is the owner's age public key: secrets and backups are
	// sealed to it as well as to the server, so the owner can open them
	// without the server.
	OwnerKey string `toml:"owner_key"`
	// Tokens are the SHA-256 of the API's tokens.
	OperatorToken string `toml:"operator_token_sha256"`
	AdminToken    string `toml:"admin_token_sha256"`
	// Releases are the release images to deploy, one a project.
	Releases []string `toml:"releases"`
	// Registries sign in to pull from, by host ("ghcr.io").
	Registries map[string]Credentials `toml:"registries"`
	Backups    Backups                `toml:"backups"`
	Updates    Updates                `toml:"updates"`
	Alerts     Alerts                 `toml:"alerts"`
	Ingress    ingress.Config         `toml:"ingress"`
	// Every is the time between polls of the release images.
	Every Duration `toml:"poll_every"`
}

type Alerts struct {
	// SMTP sends the owner email; without it, problems show only in
	// hakobu status.
	SMTP *alert.SMTP `toml:"smtp"`
	// Pulse is fetched every 15 minutes while hakobu runs (a dead man's
	// switch, such as healthchecks.io, mails when it stops); PulseFail
	// instead while a problem is open.
	Pulse     string `toml:"pulse_url"`
	PulseFail string `toml:"pulse_fail_url"`
	// AllowPorts may listen beyond loopback and the tailnet: 80 and 443
	// for a server that serves the web itself, 22 for SSH on every
	// address (allowed by itself with the API on loopback).
	AllowPorts []int `toml:"allow_ports"`
}

// Updates are hakobu's own: which newer releases the server installs by
// itself, and when.
type Updates struct {
	// Auto is "patch" (the default: 1.2.x of the 1.2 running), "minor"
	// (any 1.x) or "none"; a newer one waits for hakobu server update.
	Auto string `toml:"auto"`
	// At is the time of day (UTC) it looks: default an hour after the
	// backups start.
	At string `toml:"at"`
}

type Credentials struct {
	Username string `toml:"username"`
	Password string `toml:"password"`
}

type Backups struct {
	Primary *Bucket  `toml:"primary"`
	Copies  []Bucket `toml:"copies"`
	// At is the time of day (UTC) the daily backups start, "03:00".
	At     string `toml:"at"`
	Daily  int    `toml:"keep_daily"`
	Weekly int    `toml:"keep_weekly"`
}

type Bucket struct {
	Name      string `toml:"name"`
	Endpoint  string `toml:"endpoint"`
	Region    string `toml:"region"`
	Bucket    string `toml:"bucket"`
	AccessKey string `toml:"access_key"`
	SecretKey string `toml:"secret_key"`
	// Prefix is where this server's backups go in the bucket; default
	// "hakobu/<name>/".
	Prefix string `toml:"prefix"`
}

// Duration is a duration written like "30s".
type Duration time.Duration

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

var (
	namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	hexPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	atPattern   = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
)

// LoadConfig reads and checks the config, filling in defaults.
func LoadConfig(file string) (*Config, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	return ParseConfig(b)
}

func ParseConfig(b []byte) (*Config, error) {
	var c Config
	md, err := toml.Decode(string(b), &c)
	if err != nil {
		return nil, err
	}
	if keys := md.Undecoded(); len(keys) > 0 {
		names := make([]string, len(keys))
		for i, k := range keys {
			names[i] = k.String()
		}
		return nil, fmt.Errorf("unknown keys: %s", strings.Join(names, ", "))
	}
	if c.DataDir == "" {
		c.DataDir = "/var/lib/hakobu"
	}
	if c.Every == 0 {
		c.Every = Duration(30 * time.Second)
	}
	if c.Backups.At == "" {
		c.Backups.At = "03:00"
	}
	if c.Backups.Daily == 0 {
		c.Backups.Daily = 7
	}
	if c.Backups.Weekly == 0 {
		c.Backups.Weekly = 4
	}
	if c.Updates.Auto == "" {
		c.Updates.Auto = update.AutoPatch
	}
	if c.Updates.At == "" && atPattern.MatchString(c.Backups.At) {
		var h, m int
		_, _ = fmt.Sscanf(c.Backups.At, "%d:%d", &h, &m)
		c.Updates.At = fmt.Sprintf("%02d:%02d", (h+1)%24, m)
	}
	for _, b := range c.buckets() {
		if b.Prefix == "" {
			b.Prefix = "hakobu/" + c.Name + "/"
		}
	}
	return &c, c.check()
}

func (c *Config) buckets() []*Bucket {
	var out []*Bucket
	if c.Backups.Primary != nil {
		out = append(out, c.Backups.Primary)
	}
	for i := range c.Backups.Copies {
		out = append(out, &c.Backups.Copies[i])
	}
	return out
}

func (c *Config) check() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if !namePattern.MatchString(c.Name) {
		bad("name: %q isn't a name (lowercase letters, digits and -)", c.Name)
	}
	if c.Listen == "" {
		bad("listen: the API needs an address on the tailnet, like 100.64.0.1:7070, or 127.0.0.1:7070")
	} else if host := strings.Split(c.Listen, ":")[0]; host == "" || host == "0.0.0.0" {
		bad("listen: %s would listen on every address, the public one too; give the tailnet one", c.Listen)
	}
	if _, err := age.ParseX25519Recipient(c.OwnerKey); err != nil {
		bad("owner_key: %q isn't an age public key (age1…)", c.OwnerKey)
	}
	for _, t := range []struct{ name, v string }{{"operator_token_sha256", c.OperatorToken}, {"admin_token_sha256", c.AdminToken}} {
		if !hexPattern.MatchString(t.v) {
			bad("%s: should be a SHA-256 in hex (hakobu server token makes one)", t.name)
		}
	}
	if c.OperatorToken != "" && c.OperatorToken == c.AdminToken {
		bad("operator and admin tokens are the same")
	}
	for _, r := range c.Releases {
		ref, err := registry.ParseRef(r)
		if err != nil {
			bad("releases: %v", err)
		} else if ref.Digest != "" {
			bad("releases: %s is pinned to a digest, so it would never change; give its tag", r)
		}
	}
	if !atPattern.MatchString(c.Backups.At) {
		bad("backups.at: %q should be a time of day like 03:00", c.Backups.At)
	}
	switch c.Updates.Auto {
	case update.AutoPatch, update.AutoMinor, update.AutoNone:
	default:
		bad("updates.auto: %q should be patch, minor or none", c.Updates.Auto)
	}
	if !atPattern.MatchString(c.Updates.At) {
		bad("updates.at: %q should be a time of day like 04:00", c.Updates.At)
	}
	if err := c.Ingress.Check(); err != nil {
		errs = append(errs, err)
	}
	if m := c.Alerts.SMTP; m != nil && (m.Host == "" || m.Port == 0 || m.From == "" || m.To == "") {
		bad("alerts.smtp: needs host, port, from and to")
	}
	for _, u := range []string{c.Alerts.Pulse, c.Alerts.PulseFail} {
		if u != "" && !strings.HasPrefix(u, "https://") {
			bad("alerts: %s should be https://", u)
		}
	}
	if c.Backups.Primary == nil && len(c.Backups.Copies) > 0 {
		bad("backups: copies but no primary")
	}
	for _, b := range c.buckets() {
		if b.Endpoint == "" || b.Bucket == "" || b.AccessKey == "" || b.SecretKey == "" {
			bad("backups: bucket %q needs endpoint, bucket, access_key and secret_key", b.Name)
		}
		if !strings.HasPrefix(b.Endpoint, "https://") && !strings.HasPrefix(b.Endpoint, "http://127.0.0.1") {
			bad("backups: bucket %q: the endpoint should be https://", b.Name)
		}
	}
	return errors.Join(errs...)
}

// LoopbackAPI tells whether the API is on loopback, reached through SSH.
func (c *Config) LoopbackAPI() bool { return strings.HasPrefix(c.Listen, "127.0.0.1:") }

// Paths in the data directory.
func (c *Config) KeyFile() string     { return filepath.Join(c.DataDir, "key.age") }
func (c *Config) StateFile() string   { return filepath.Join(c.DataDir, "state.json") }
func (c *Config) WatchFile() string   { return filepath.Join(c.DataDir, "watch.json") }
func (c *Config) DeploysFile() string { return filepath.Join(c.DataDir, "deploys.jsonl") }
func (c *Config) Snapshots() string   { return filepath.Join(c.DataDir, "snapshots") }
func (c *Config) TempDir() string     { return filepath.Join(c.DataDir, "tmp") }
func (c *Config) AlertsFile() string  { return filepath.Join(c.DataDir, "alerts.json") }
func (c *Config) CertDir() string     { return filepath.Join(c.DataDir, "certs") }

// Level is what a token may do.
type Level int

const (
	None Level = iota
	Operator
	Admin
)

// TokenLevel is what the token may do, comparing hashes in constant time.
func (c *Config) TokenLevel(token string) Level {
	if token == "" {
		return None
	}
	sum := sha256.Sum256([]byte(token))
	h := []byte(hex.EncodeToString(sum[:]))
	if subtle.ConstantTimeCompare(h, []byte(c.AdminToken)) == 1 {
		return Admin
	}
	if subtle.ConstantTimeCompare(h, []byte(c.OperatorToken)) == 1 {
		return Operator
	}
	return None
}

// HashToken is what the config keeps of a token.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
