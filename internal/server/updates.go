package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/hakobudev/hakobu/internal/update"
)

// hakobu's own releases: the daemon looks for a newer one each day, shows
// it in status, installs one updates.auto allows at updates.at, and asks
// root to install it (update.RequestFile), which it can't do itself.

// UpdateUnit watches for update requests; an install from before updates
// has none, and a request would wait forever.
var UpdateUnit = "/etc/systemd/system/hakobu-update.path"

// behindFor is how long a newer minor release may wait for an update by
// hand before the owner hears of it.
const behindFor = 14 * 24 * time.Hour

// checkReleases looks for hakobu's latest release now and each day after,
// and says how the last update went.
func (d *Daemon) checkReleases(ctx context.Context) {
	d.updateAlerts()
	if d.LatestRelease == nil {
		return
	}
	for {
		d.refreshLatest()
		d.updateAlerts()
		select {
		case <-ctx.Done():
			return
		case <-time.After(24 * time.Hour):
		}
	}
}

func (d *Daemon) refreshLatest() update.Release {
	r, err := d.LatestRelease()
	d.mu.Lock()
	defer d.mu.Unlock()
	if err == nil {
		d.latest = r
	}
	return d.latest
}

// updateDaily installs, at updates.at each day, the latest release if
// updates.auto allows it.
func (d *Daemon) updateDaily(ctx context.Context) {
	if d.Config.Updates.Auto == update.AutoNone || d.LatestRelease == nil {
		return
	}
	for {
		t := time.NewTimer(time.Until(nextAt(time.Now(), d.Config.Updates.At)))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		if msg := d.autoUpdate(); msg != "" {
			fmt.Fprintln(d.log(), "update:", msg)
		}
	}
}

// autoUpdate asks for the latest release if updates.auto allows it and
// nothing's in the way, and says what it did or why not ("": nothing new).
func (d *Daemon) autoUpdate() string {
	latest := d.refreshLatest().Tag
	if !update.Allowed(d.Config.Updates.Auto, latest, d.Version) {
		return ""
	}
	if st, ok := d.lastUpdate(); ok && st.To == latest && (st.State == "rolled back" || st.State == "failed") {
		return fmt.Sprintf("%s didn't install last time (%s): not trying again by itself", latest, st.State)
	}
	d.mu.Lock()
	busy := d.backingUp
	d.mu.Unlock()
	if busy || d.Applier.Busy() {
		return fmt.Sprintf("%s waits for tomorrow: a deploy or the backups are running", latest)
	}
	if err := d.askUpdate(latest); err != nil {
		return err.Error()
	}
	return "asked for " + latest
}

func (d *Daemon) lastUpdate() (update.Status, bool) {
	if d.InstallDir == "" {
		return update.Status{}, false
	}
	return update.ReadStatus(d.InstallDir)
}

// updateAlerts tells the owner of an update that went wrong, and of a newer
// minor release that's waited long for an update by hand.
func (d *Daemon) updateAlerts() {
	st, ok := d.lastUpdate()
	failed := ok && (st.State == "rolled back" || st.State == "failed") && update.Newer(st.To, d.Version)
	subject := ""
	if failed {
		subject = fmt.Sprintf("hakobu couldn't update to %s", st.To)
	}
	d.raise("hakobu:update", failed, subject, st.Message+"\n\nhakobu "+update.Tag(d.Version)+" runs; journalctl -u hakobu-update on the server says more.")

	d.mu.Lock()
	latest := d.latest
	d.mu.Unlock()
	behind := update.Behind(latest.Tag, d.Version) && !latest.Published.IsZero() && time.Since(latest.Published) > behindFor
	subject = ""
	if behind {
		subject = fmt.Sprintf("hakobu %s has been out for %d days", latest.Tag, int(time.Since(latest.Published).Hours()/24))
	}
	d.raise("hakobu:behind", behind, subject, fmt.Sprintf("This server runs hakobu %s. To update: hakobu server update %s", update.Tag(d.Version), d.Config.Name))
}

// updateStatus is what status says of hakobu itself: its version, a newer
// release if there's one, and the last update.
func (d *Daemon) updateStatus(s *Status) {
	s.Version = update.Tag(d.Version)
	d.mu.Lock()
	if update.Newer(d.latest.Tag, d.Version) {
		s.Latest = d.latest.Tag
	}
	d.mu.Unlock()
	if st, ok := d.lastUpdate(); ok {
		s.LastUpdate = &st
	}
}

type updateRequest struct {
	To string `json:"to"` // a release's tag; the latest if empty
}

// requestUpdate asks root to install a newer release; it answers at once,
// and hakobu restarts as the update goes on (status says how it went).
func (d *Daemon) requestUpdate(w http.ResponseWriter, r *http.Request) (any, error) {
	var req updateRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	to := ""
	if req.To != "" {
		to = update.Tag(req.To)
		if !update.Newer(to, d.Version) {
			return nil, badRequest("hakobu %s runs here: an update goes only to a newer release, not %s", update.Tag(d.Version), req.To)
		}
	}
	if err := d.askUpdate(to); err != nil {
		return nil, err
	}
	if to == "" {
		to = "latest"
	}
	return map[string]string{"from": update.Tag(d.Version), "to": to}, nil
}

// askUpdate asks root, through update.RequestFile, to install release to
// (the latest if "").
func (d *Daemon) askUpdate(to string) error {
	if _, err := os.Stat(UpdateUnit); err != nil {
		return apiError{http.StatusConflict, "this server's install has no updater yet: run install.sh on it once (docs/updates.md)"}
	}
	if st, ok := d.lastUpdate(); ok && st.State == "running" && time.Since(st.At) < 30*time.Minute {
		return apiError{http.StatusConflict, fmt.Sprintf("an update to %s is running", st.To)}
	}
	p := filepath.Join(d.Config.DataDir, update.RequestFile)
	if err := os.WriteFile(p+".tmp", []byte(to), 0o600); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}
