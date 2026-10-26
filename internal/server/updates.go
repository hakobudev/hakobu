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
// it in status, and asks root to install one (update.RequestFile), which
// it can't do itself.

// UpdateUnit watches for update requests; an install from before updates
// has none, and a request would wait forever.
var UpdateUnit = "/etc/systemd/system/hakobu-update.path"

// checkReleases looks for hakobu's latest release now and each day after.
func (d *Daemon) checkReleases(ctx context.Context) {
	if d.LatestRelease == nil {
		return
	}
	for {
		if tag, err := d.LatestRelease(); err == nil {
			d.mu.Lock()
			d.latest = tag
			d.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(24 * time.Hour):
		}
	}
}

// updateStatus is what status says of hakobu itself: its version, a newer
// release if there's one, and the last update.
func (d *Daemon) updateStatus(s *Status) {
	s.Version = update.Tag(d.Version)
	d.mu.Lock()
	if update.Newer(d.latest, d.Version) {
		s.Latest = d.latest
	}
	d.mu.Unlock()
	if d.InstallDir != "" {
		if st, ok := update.ReadStatus(d.InstallDir); ok {
			s.LastUpdate = &st
		}
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
	if _, err := os.Stat(UpdateUnit); err != nil {
		return nil, apiError{http.StatusConflict, "this server's install has no updater yet: run install.sh on it once (docs/updates.md)"}
	}
	if d.InstallDir != "" {
		if st, ok := update.ReadStatus(d.InstallDir); ok && st.State == "running" && time.Since(st.At) < 30*time.Minute {
			return nil, apiError{http.StatusConflict, fmt.Sprintf("an update to %s is running", st.To)}
		}
	}
	p := filepath.Join(d.Config.DataDir, update.RequestFile)
	if err := os.WriteFile(p+".tmp", []byte(to), 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(p+".tmp", p); err != nil {
		return nil, err
	}
	if to == "" {
		to = "latest"
	}
	return map[string]string{"from": update.Tag(d.Version), "to": to}, nil
}
