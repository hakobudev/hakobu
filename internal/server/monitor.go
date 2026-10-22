package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hakobudev/hakobu/internal/apply"
	"github.com/hakobudev/hakobu/internal/audit"
	"github.com/hakobudev/hakobu/internal/deploy"
	"github.com/hakobudev/hakobu/internal/node"
)

// Watcher is what watching the server needs of it (node.Local).
type Watcher interface {
	CheckHealth(ctx context.Context, app node.AppSpec, port int64) (ok bool, why string, checked bool)
	Disk(ctx context.Context) (used, total uint64, err error)
	PostgresReady(ctx context.Context) bool
}

// How often each check runs, and how many failed health checks in a row
// make an app down (a deploy's restart or a slow request isn't one).
const (
	healthEvery  = time.Minute
	healthMisses = 3
	diskEvery    = 10 * time.Minute
	auditEvery   = time.Hour
	pulseEvery   = 15 * time.Minute
)

// Disk use at which an alert opens, and below which it closes again.
const diskHigh, diskOK = 0.85, 0.80

// monitor runs the checks until ctx ends.
func (d *Daemon) monitor(ctx context.Context) {
	misses := map[string]int{}
	var lastDisk, lastAudit, lastPulse time.Time
	t := time.NewTicker(healthEvery)
	defer t.Stop()
	for {
		d.checkApps(ctx, misses)
		now := time.Now()
		if now.Sub(lastDisk) >= diskEvery {
			d.checkDisk(ctx)
			d.checkPostgres(ctx)
			lastDisk = now
		}
		if now.Sub(lastAudit) >= auditEvery {
			d.Audit(ctx)
			lastAudit = now
		}
		if now.Sub(lastPulse) >= pulseEvery {
			d.pulse(ctx)
			lastPulse = now
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (d *Daemon) raise(key string, bad bool, subject, body string) {
	if d.Alerts == nil {
		return
	}
	if err := d.Alerts.Set(key, bad, subject, body); err != nil {
		fmt.Fprintln(d.log(), "alert:", err)
	}
}

// checkApps health-checks every live app; healthMisses failures in a row
// open an alert, one answer closes it.
func (d *Daemon) checkApps(ctx context.Context, misses map[string]int) {
	w, ok := d.Machine.(Watcher)
	if !ok {
		return
	}
	apps, err := d.Applier.Apps()
	if err != nil {
		return
	}
	specs, _ := d.Applier.Specs()
	for project, as := range apps {
		for name, a := range as {
			if a.Slot == "" {
				continue
			}
			health := healthPath(specs, project, name)
			app := node.AppSpec{Name: apply.AppKey(project, name), Project: project, ActiveSlot: a.Slot, HealthPath: health}
			ok, why, checked := w.CheckHealth(ctx, app, a.Port)
			if !checked && why == "" {
				why = "its container isn't running"
			}
			key := "app:" + project + "/" + name
			if ok {
				misses[key] = 0
				d.raise(key, false, "", "")
				continue
			}
			misses[key]++
			if misses[key] >= healthMisses {
				logs, _ := d.Machine.Logs(ctx, app.Live(), 30)
				d.raise(key, true, fmt.Sprintf("%s/%s is down: %s", project, name, why),
					fmt.Sprintf("%s/%s hasn't answered %s for %d minutes: %s.\n\nIts last output:\n%s", project, name, health, misses[key], why, logs))
			}
		}
	}
}

func healthPath(specs map[string]map[string]string, project, app string) string {
	if h := specs[project][app]; h != "" {
		return h
	}
	return "/up"
}

func (d *Daemon) checkDisk(ctx context.Context) {
	w, ok := d.Machine.(Watcher)
	if !ok {
		return
	}
	used, total, err := w.Disk(ctx)
	if err != nil || total == 0 {
		return
	}
	share := float64(used) / float64(total)
	switch {
	case share >= diskHigh:
		d.raise("disk", true, fmt.Sprintf("the disk is %.0f%% full", share*100),
			fmt.Sprintf("%.1f of %.1f GB used. A full disk stops the databases and deploys; old images and logs are the usual culprits (docker system df).", float64(used)/(1<<30), float64(total)/(1<<30)))
	case share < diskOK:
		d.raise("disk", false, "", "")
	}
}

func (d *Daemon) checkPostgres(ctx context.Context) {
	w, ok := d.Machine.(Watcher)
	if !ok {
		return
	}
	res, err := d.Applier.Resources()
	if err != nil {
		return
	}
	has := false
	for _, r := range res {
		has = has || r.Kind == "database"
	}
	if !has {
		return
	}
	down := !w.PostgresReady(ctx)
	d.raise("postgres", down, "PostgreSQL doesn't answer", "The server's PostgreSQL ("+node.PostgresContainer+") doesn't answer: the apps that use it fail. docker logs "+node.PostgresContainer+" says why.")
}

// Audit compares the server with what it should be, opening an alert for
// each thing out of place and closing those that are fixed.
func (d *Daemon) Audit(ctx context.Context) []audit.Finding {
	var found []audit.Finding
	allow := append(d.Config.Ingress.Ports(), d.Config.Alerts.AllowPorts...)
	if ports, err := audit.HostPorts(allow); err == nil {
		found = append(found, ports...)
	}
	if d.Containers != nil {
		if names, err := d.Containers(ctx); err == nil {
			ours := d.ours()
			found = append(found, audit.Containers(names, ours)...)
		}
	}
	found = append(found, audit.Upgrades(audit.AptStamp, time.Now(), 72*time.Hour)...)
	if d.Alerts == nil {
		return found
	}
	now := map[string]bool{}
	for _, f := range found {
		now["audit:"+f.Key] = true
		d.raise("audit:"+f.Key, true, f.Problem, f.Problem+".\n\nhakobu checks the server every hour; this alert closes once it's put right.")
	}
	open, _ := d.Alerts.Keys("audit:")
	for _, k := range open {
		if !now[k] {
			d.raise(k, false, "", "")
		}
	}
	return found
}

// ours tells hakobu's containers from others: the apps' (both slots, the
// worker, a release command), the caches, Postgres and cloudflared.
func (d *Daemon) ours() func(string) bool {
	names := map[string]bool{node.PostgresContainer: true, "hakobu-cloudflared": true}
	apps, _ := d.Applier.Apps()
	for project, as := range apps {
		for name := range as {
			key := apply.AppKey(project, name)
			for _, suffix := range []string{"-blue", "-green", "-worker", "-release"} {
				names[key+suffix] = true
			}
		}
	}
	res, _ := d.Applier.Resources()
	for _, r := range res {
		if r.Kind == "cache" {
			names[apply.CacheKey(r.Project, r.Name)] = true
		}
	}
	return func(n string) bool { return names[n] }
}

// RunningContainers names the running containers.
func RunningContainers(ctx context.Context) ([]string, error) {
	cs, err := deploy.RunningContainers(ctx)
	var names []string
	for _, c := range cs {
		names = append(names, c.Name)
	}
	return names, err
}

// pulse tells the dead man's switch hakobu runs, and whether all's well.
func (d *Daemon) pulse(ctx context.Context) {
	url := d.Config.Alerts.Pulse
	if d.Alerts != nil && d.Config.Alerts.PulseFail != "" {
		if open, _ := d.Alerts.Open(); len(open) > 0 {
			url = d.Config.Alerts.PulseFail
		}
	}
	if url == "" {
		return
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		fmt.Fprintln(d.log(), "pulse:", err)
		return
	}
	resp.Body.Close()
}

// deployDone mails a failed deploy and closes the alert on the next one
// that works.
func (d *Daemon) deployDone(m apply.Meta, r apply.Result) {
	key := "deploy:" + r.Project
	if r.Project == "" {
		key = "deploy:" + r.Release
	}
	subject := fmt.Sprintf("deploy of %s failed: %s", r.Project, firstLine(r.Error))
	body := fmt.Sprintf("Release %s (commit %s of %s) didn't go live; the live version keeps running.\n\n%s\n\n%s", r.Release, r.Revision, r.Source, r.Error, r.Log)
	d.raise(key, r.Error != "", subject, body)
}

// backupsDone mails failed backups, and closes the alert once a run works.
func (d *Daemon) backupsDone(run BackupRun) {
	d.raise("backups", len(run.Errors) > 0, fmt.Sprintf("%d backups failed", len(run.Errors)),
		"The daily backups didn't all go through:\n\n"+strings.Join(run.Errors, "\n")+"\n\nMade:\n"+strings.Join(run.Made, "\n"))
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
