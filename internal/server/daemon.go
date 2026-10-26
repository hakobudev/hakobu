package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"filippo.io/age"
	"github.com/hakobudev/hakobu/internal/alert"
	"github.com/hakobudev/hakobu/internal/apply"
	"github.com/hakobudev/hakobu/internal/backups"
	"github.com/hakobudev/hakobu/internal/deploy"
	"github.com/hakobudev/hakobu/internal/ingress"
	"github.com/hakobudev/hakobu/internal/node"
	"github.com/hakobudev/hakobu/internal/registry"
	"github.com/hakobudev/hakobu/internal/s3"
	"github.com/hakobudev/hakobu/internal/seal"
	"github.com/hakobudev/hakobu/internal/update"
	"github.com/hakobudev/hakobu/internal/status"
)

// Machine is what the daemon needs of the server beyond deploys: the
// apps' logs, and starting proxies and dropping data (node.Local).
type Machine interface {
	apply.Node
	apply.Destroyer
	apply.Proxier
	apply.Reconciler
	backups.Source
	Logs(ctx context.Context, container string, lines int) (string, error)
}

// Daemon is hakobu on a server.
type Daemon struct {
	Config  *Config
	Machine Machine
	Applier *apply.Applier
	Watch   *apply.Watch
	// Vault is nil when no bucket is configured: then nothing is backed
	// up, and status says so.
	Vault *backups.Vault
	// Alerts mail the owner; with no mail server they're only in status.
	Alerts *alert.Alerts
	// Containers names the running containers, for the audit.
	Containers func(ctx context.Context) ([]string, error)
	Log        io.Writer
	// Version is hakobu's own; InstallDir holds its binary and the last
	// update's status; LatestRelease finds hakobu's newest release (nil:
	// none is looked for).
	Version       string
	InstallDir    string
	LatestRelease func() (update.Release, error)

	mu         sync.Mutex
	lastBackup BackupRun
	latest     update.Release // hakobu's newest release, as last found
	backingUp  bool
	restarting bool

	settingsMu sync.Mutex    // one change of the settings at a time
	restart    chan struct{} // the settings changed
}

// BackupRun is how the last daily backups went.
type BackupRun struct {
	At     time.Time `json:"at"`
	Made   []string  `json:"made"`
	Errors []string  `json:"errors"`
}

// New builds the daemon from its config, with the server's key from the
// data directory: one that's missing is an error, never made anew here,
// as a new key couldn't open what the old one sealed.
func New(c *Config, m Machine, log io.Writer) (*Daemon, error) {
	for _, dir := range []string{c.DataDir, c.Snapshots(), c.TempDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	id, err := seal.LoadIdentity(c.KeyFile())
	if err != nil {
		return nil, fmt.Errorf("the server's key: %w (hakobu server init makes one)", err)
	}
	auth := map[string]deploy.RegistryAuth{}
	creds := map[string]registry.Credentials{}
	for host, cr := range c.Registries {
		auth[host] = deploy.RegistryAuth{Username: cr.Username, Password: cr.Password}
		creds[host] = registry.Credentials{Username: cr.Username, Password: cr.Password}
	}
	a := &apply.Applier{Node: m, Server: c.Name, StateFile: c.StateFile(), SnapshotDir: c.Snapshots(), Auth: auth}
	var refs []registry.Ref
	for _, r := range c.Releases {
		ref, err := registry.ParseRef(r)
		if err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	w := &apply.Watch{
		Applier: a, Registry: &registry.Client{Auth: creds}, Releases: refs, Identity: id,
		Every: time.Duration(c.Every), WatchFile: c.WatchFile(), DeploysFile: c.DeploysFile(), Log: log,
	}
	w.ReportTo(status.GitHub{})
	d := &Daemon{Config: c, Machine: m, Applier: a, Watch: w, Log: log, Containers: RunningContainers,
		Alerts: &alert.Alerts{Server: c.Name, File: c.AlertsFile()}, restart: make(chan struct{}, 1)}
	if c.Alerts.SMTP != nil {
		d.Alerts.Mailer = *c.Alerts.SMTP
	}
	report := w.Done
	w.Done = func(m apply.Meta, r apply.Result) {
		report(m, r)
		d.deployDone(m, r)
	}
	if p := c.Backups.Primary; p != nil {
		d.Vault = &backups.Vault{
			Source: m, Primary: target(*p), Recipients: []string{id.Recipient().String(), c.OwnerKey},
			Identity: id, TempDir: c.TempDir(),
		}
		for _, b := range c.Backups.Copies {
			d.Vault.Copies = append(d.Vault.Copies, target(b))
		}
	}
	return d, nil
}

func target(b Bucket) backups.Target {
	name := b.Name
	if name == "" {
		name = b.Bucket
	}
	return backups.Target{Name: name, Store: s3.New(b.Endpoint, b.Region, b.Bucket, b.AccessKey, b.SecretKey), Prefix: b.Prefix}
}

// Run tidies up after an unfinished deploy and starts the apps' proxies,
// then polls, backs up and serves the API
// until ctx ends.
func (d *Daemon) Run(ctx context.Context) error {
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	if d.restart == nil {
		d.restart = make(chan struct{}, 1)
	}
	go d.restartWhenAsked(ctx, stop)
	if err := d.Applier.Reconcile(ctx, d.Machine); err != nil {
		return err
	}
	if err := d.Applier.EnsureProxies(ctx, d.Machine); err != nil {
		return err
	}
	d.closeGoneReleases()
	l, err := net.Listen("tcp", d.Config.Listen)
	if err != nil {
		return fmt.Errorf("the API can't listen on %s: %w (is the server in the tailnet?)", d.Config.Listen, err)
	}
	srv := &http.Server{Handler: d.API(), ReadHeaderTimeout: 10 * time.Second}
	go d.Watch.Run(ctx)
	go d.backupDaily(ctx)
	go d.monitor(ctx)
	go d.checkReleases(ctx)
	go d.updateDaily(ctx)
	go func() {
		if err := ingress.Serve(ctx, d.Config.Ingress, d.Applier.Domains, d.Config.CertDir(), d.log()); err != nil {
			fmt.Fprintln(d.log(), err)
			d.raise("ingress", true, "the apps can't be reached: "+err.Error(), err.Error())
		}
	}()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	fmt.Fprintf(d.log(), "hakobu %s: API on %s, watching %d release images\n", d.Config.Name, d.Config.Listen, len(d.Config.Releases))
	if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.restarting {
		return ErrRestart
	}
	return nil
}

// closeGoneReleases closes the failed-deploy alerts of release images the
// config no longer has: no deploy of theirs would ever close them.
func (d *Daemon) closeGoneReleases() {
	if d.Alerts == nil {
		return
	}
	keys, _ := d.Alerts.Keys("deploy:")
	for _, k := range keys {
		image := strings.TrimPrefix(k, "deploy:")
		if !slices.ContainsFunc(d.Watch.Releases, func(r registry.Ref) bool { return r.Host+"/"+r.Repo == image }) {
			d.raise(k, false, "", "")
		}
	}
}

// backupDaily backs everything up at the configured time each day.
func (d *Daemon) backupDaily(ctx context.Context) {
	if d.Vault == nil {
		fmt.Fprintln(d.log(), "warning: no backup bucket configured: nothing is backed up")
		return
	}
	for {
		t := time.NewTimer(time.Until(nextAt(time.Now(), d.Config.Backups.At)))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		run := d.BackupAll(ctx)
		for _, e := range run.Errors {
			fmt.Fprintln(d.log(), "backup:", e)
		}
	}
}

// nextAt is the next time of day at ("03:00", UTC) after now.
func nextAt(now time.Time, at string) time.Time {
	var h, m int
	_, _ = fmt.Sscanf(at, "%d:%d", &h, &m) // checked by the config
	now = now.UTC()
	next := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, time.UTC)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// BackupAll backs up every database and volume the projects have in their
// files, then rotates each project's backups.
func (d *Daemon) BackupAll(ctx context.Context) BackupRun {
	d.mu.Lock()
	d.backingUp = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.backingUp = false
		d.mu.Unlock()
	}()
	run := BackupRun{At: time.Now().UTC()}
	resources, err := d.Applier.Resources()
	if err != nil {
		run.Errors = append(run.Errors, err.Error())
	}
	projects := map[string]bool{}
	for _, r := range resources {
		if r.Kind == "cache" {
			continue // a cache holds nothing that can't be rebuilt
		}
		projects[r.Project] = true
		m, err := d.backup(ctx, r)
		if m.SHA256 != "" {
			run.Made = append(run.Made, m.String())
		}
		if err != nil {
			run.Errors = append(run.Errors, fmt.Sprintf("%s/%s: %v", r.Project, r.Name, err))
		}
	}
	for p := range projects {
		if _, err := d.Vault.Rotate(ctx, p, d.Config.Backups.Daily, d.Config.Backups.Weekly); err != nil {
			run.Errors = append(run.Errors, fmt.Sprintf("%s: rotating: %v", p, err))
		}
	}
	d.mu.Lock()
	d.lastBackup = run
	d.mu.Unlock()
	d.backupsDone(run)
	return run
}

func (d *Daemon) backup(ctx context.Context, r apply.Resource) (backups.Manifest, error) {
	if d.Vault == nil {
		return backups.Manifest{}, errors.New("no backup bucket configured")
	}
	if r.Kind == "database" {
		return d.Vault.BackupDatabase(ctx, r.Project, r.Name, r.DB)
	}
	app, _, _ := strings.Cut(r.Name, "/")
	return d.Vault.BackupVolume(ctx, r.Project, app, r.App, r.Volume)
}

func (d *Daemon) log() io.Writer {
	if d.Log != nil {
		return d.Log
	}
	return io.Discard
}

// InitKey makes the server's key if it has none, and returns its public
// half, which goes in projects' hakobu.secrets.toml.
func InitKey(c *Config) (string, error) {
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return "", err
	}
	if id, err := seal.LoadIdentity(c.KeyFile()); err == nil {
		return id.Recipient().String(), nil
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return "", err
	}
	if err := seal.SaveIdentity(c.KeyFile(), id); err != nil {
		return "", err
	}
	return id.Recipient().String(), nil
}

var _ Machine = node.Local{}

func sealIdentity(c *Config) (string, error) {
	id, err := seal.LoadIdentity(c.KeyFile())
	if err != nil {
		return "", err
	}
	return id.Recipient().String(), nil
}
