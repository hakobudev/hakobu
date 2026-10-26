package server

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"github.com/hakobudev/hakobu/internal/seal"
)

// A PendingSecret is one a person sealed on the server (ssh … secret
// set), kept for the agent to put in the project's hakobu.secrets.toml:
// the value never goes through the agent, sealed or not it can't read it.
type PendingSecret struct {
	Project string    `json:"project"`
	App     string    `json:"app"` // "hakobu" for hakobu's own
	Name    string    `json:"name"`
	Sealed  string    `json:"sealed"`
	At      time.Time `json:"at"`
}

// pendingFor is how long one waits for the agent.
const pendingFor = 7 * 24 * time.Hour

var projectName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

func (c *Config) pendingFile() string { return filepath.Join(c.DataDir, "pending-secrets.json") }

func (d *Daemon) loadPending() ([]PendingSecret, error) {
	b, err := os.ReadFile(d.Config.pendingFile())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ps []PendingSecret
	if err := json.Unmarshal(b, &ps); err != nil {
		return nil, err
	}
	return slices.DeleteFunc(ps, func(p PendingSecret) bool { return time.Since(p.At) > pendingFor }), nil
}

func (d *Daemon) savePending(ps []PendingSecret) error {
	b, err := json.Marshal(ps)
	if err != nil {
		return err
	}
	tmp := d.Config.pendingFile() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, d.Config.pendingFile())
}

// addPending keeps a sealed secret, in place of one for the same
// variable: it must open with the server's key.
func (d *Daemon) addPending(w http.ResponseWriter, r *http.Request) (any, error) {
	var p PendingSecret
	if err := decode(r, &p); err != nil {
		return nil, err
	}
	if !projectName.MatchString(p.Project) || p.App == "" || p.Name == "" {
		return nil, badRequest("give the project, the app and the variable")
	}
	// The same checks hakobu secret set makes.
	s, err := seal.New(d.Config.OwnerKey)
	if err != nil {
		return nil, err
	}
	if err := s.Set(p.App, p.Name, "x"); err != nil {
		return nil, badRequest("%v", err)
	}
	id, err := seal.LoadIdentity(d.Config.KeyFile())
	if err != nil {
		return nil, err
	}
	if _, err := seal.OpenValue(p.Sealed, id); err != nil {
		return nil, badRequest("the value isn't sealed to this server: %v", err)
	}
	p.At = time.Now().UTC()
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()
	ps, err := d.loadPending()
	if err != nil {
		return nil, err
	}
	ps = slices.DeleteFunc(ps, func(q PendingSecret) bool {
		return q.Project == p.Project && q.App == p.App && q.Name == p.Name
	})
	if err := d.savePending(append(ps, p)); err != nil {
		return nil, err
	}
	return map[string]string{"project": p.Project, "app": p.App, "name": p.Name}, nil
}

// pending are a project's sealed secrets waiting for the agent; with
// clear (POST), they're forgotten, as they're in hakobu.secrets.toml now.
func (d *Daemon) pending(w http.ResponseWriter, r *http.Request) (any, error) {
	project := r.URL.Query().Get("project")
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()
	ps, err := d.loadPending()
	if err != nil {
		return nil, err
	}
	var out, keep []PendingSecret
	for _, p := range ps {
		if project == "" || p.Project == project {
			out = append(out, p)
		} else {
			keep = append(keep, p)
		}
	}
	if r.Method == http.MethodPost {
		if err := d.savePending(keep); err != nil {
			return nil, err
		}
	}
	if out == nil {
		out = []PendingSecret{}
	}
	return out, nil
}
