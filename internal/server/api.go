package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hakobudev/hakobu/internal/alert"
	"github.com/hakobudev/hakobu/internal/apply"
	"github.com/hakobudev/hakobu/internal/backups"
	"github.com/hakobudev/hakobu/internal/node"
	"github.com/hakobudev/hakobu/internal/registry"
	"github.com/hakobudev/hakobu/internal/update"
)

// API is the daemon's HTTP API, for the CLI and its MCP server. Every
// call needs a token: the operator's sees, deploys, rolls back, backs up
// and updates hakobu; only the admin's restores and destroys.
func (d *Daemon) API() http.Handler {
	mux := http.NewServeMux()
	op, admin := d.need(Operator), d.need(Admin)
	mux.Handle("GET /v1/key", op(d.key))
	mux.Handle("GET /v1/status", op(d.status))
	mux.Handle("GET /v1/deploys", op(d.deploys))
	mux.Handle("GET /v1/logs", op(d.logs))
	mux.Handle("GET /v1/backups", op(d.listBackups))
	mux.Handle("POST /v1/deploy", op(d.deploy))
	mux.Handle("POST /v1/rollback", op(d.rollback))
	mux.Handle("POST /v1/backup", op(d.backupNow))
	mux.Handle("POST /v1/update", op(d.requestUpdate))
	mux.Handle("GET /v1/settings", op(d.settings))
	// The backups' buckets need the admin token: the handler asks.
	mux.Handle("POST /v1/settings", op(d.changeSettings))
	mux.Handle("POST /v1/secrets", op(d.addPending))
	mux.Handle("GET /v1/secrets", op(d.pending))
	mux.Handle("POST /v1/secrets/clear", op(d.pending))
	mux.Handle("POST /v1/restore", admin(d.restore))
	mux.Handle("POST /v1/destroy", admin(d.destroy))
	return mux
}

type handler func(w http.ResponseWriter, r *http.Request) (any, error)

// apiError is an error with the status to answer it with.
type apiError struct {
	status int
	msg    string
}

func (e apiError) Error() string { return e.msg }

func badRequest(format string, a ...any) error {
	return apiError{http.StatusBadRequest, fmt.Sprintf(format, a...)}
}

func (d *Daemon) need(level Level) func(handler) http.Handler {
	return func(h handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			have := d.levelOf(r)
			switch {
			case have == None:
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "no valid token"})
				return
			case have < level:
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "this needs the admin token"})
				return
			}
			v, err := h(w, r)
			if _, ok := v.(streamed); ok {
				return
			}
			if err != nil {
				status := http.StatusInternalServerError
				if e, ok := err.(apiError); ok {
					status = e.status
				}
				writeJSON(w, status, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, v)
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func decode(r *http.Request, v any) error {
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(v); err != nil {
		return badRequest("bad request body: %v", err)
	}
	return nil
}

func (d *Daemon) key(w http.ResponseWriter, r *http.Request) (any, error) {
	id, err := sealIdentity(d.Config)
	if err != nil {
		return nil, err
	}
	return map[string]string{"server": d.Config.Name, "recipient": id, "owner": d.Config.OwnerKey}, nil
}

// Status is the server at a glance.
type Status struct {
	Server     string                               `json:"server"`
	Releases   []string                             `json:"releases"`
	PollErrors []string                             `json:"poll_errors,omitempty"` // release images it can't read now
	Apps       map[string]map[string]apply.AppState `json:"apps"`
	Resources  []ResourceStatus                     `json:"resources"`
	LastDeploy *apply.Result                        `json:"last_deploy,omitempty"`
	LastBackup *BackupRun                           `json:"last_backup,omitempty"`
	Backups    bool                                 `json:"backups"` // a bucket is configured
	Problems   []alert.Problem                      `json:"problems,omitempty"`
	Mail       bool                                 `json:"mail"` // a mail server is configured
	Version    string                               `json:"version"`
	Latest     string                               `json:"latest,omitempty"` // a newer release of hakobu
	LastUpdate *update.Status                       `json:"last_update,omitempty"`
	// Down are the apps whose health checks fail ("project/app"), as the
	// open alerts say.
	Down []string `json:"down,omitempty"`
	// Deployed is each project's last deploy.
	Deployed map[string]apply.Result `json:"deployed,omitempty"`
	Host     *node.Host              `json:"host,omitempty"`
}

type ResourceStatus struct {
	Project string `json:"project"`
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Orphan  bool   `json:"orphan"`
	Size    int64  `json:"size,omitempty"`  // bytes a database takes, or a cache's memory
	Limit   int64  `json:"limit,omitempty"` // a cache's memory limit
}

// sizer measures data; a moment each, so status asks with a deadline.
type sizer interface {
	DatabaseSizes(ctx context.Context) (map[string]int64, error)
	CacheMemory(ctx context.Context, c node.CacheSpec) (used, limit int64, err error)
}

func (d *Daemon) status(w http.ResponseWriter, r *http.Request) (any, error) {
	s := Status{Server: d.Config.Name, Releases: d.Config.Releases, Backups: d.Vault != nil, Mail: d.Config.Alerts.SMTP != nil}
	if d.Alerts != nil {
		s.Problems, _ = d.Alerts.Open()
		keys, _ := d.Alerts.Keys("app:")
		for _, k := range keys {
			s.Down = append(s.Down, strings.TrimPrefix(k, "app:"))
		}
	}
	if d.Watch != nil {
		s.PollErrors = d.Watch.PollErrors()
	}
	if h, ok := d.Machine.(interface {
		Host(context.Context) node.Host
	}); ok {
		host := h.Host(r.Context())
		s.Host = &host
	}
	var err error
	if s.Apps, err = d.Applier.Apps(); err != nil {
		return nil, err
	}
	res, err := d.Applier.Resources()
	if err != nil {
		return nil, err
	}
	m, measure := d.Machine.(sizer)
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	var dbs map[string]int64
	if measure && slices.ContainsFunc(res, func(x apply.Resource) bool { return x.Kind == "database" }) {
		dbs, _ = m.DatabaseSizes(ctx)
	}
	for _, x := range res {
		rs := ResourceStatus{Project: x.Project, Kind: x.Kind, Name: x.Name, Orphan: !x.InFile}
		switch {
		case x.Kind == "database":
			rs.Size = dbs[x.DB.Name]
		case x.Kind == "cache" && measure && ctx.Err() == nil:
			rs.Size, rs.Limit, _ = m.CacheMemory(ctx, x.Cache)
		}
		s.Resources = append(s.Resources, rs)
	}
	recent, _ := apply.Deploys(d.Config.DeploysFile(), 100)
	if len(recent) > 0 {
		s.LastDeploy = &recent[0]
	}
	for _, x := range recent {
		if _, seen := s.Deployed[x.Project]; x.Project != "" && !seen {
			if s.Deployed == nil {
				s.Deployed = map[string]apply.Result{}
			}
			s.Deployed[x.Project] = x
		}
	}
	d.mu.Lock()
	if !d.lastBackup.At.IsZero() {
		b := d.lastBackup
		s.LastBackup = &b
	}
	d.mu.Unlock()
	d.updateStatus(&s)
	return s, nil
}

func (d *Daemon) deploys(w http.ResponseWriter, r *http.Request) (any, error) {
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n <= 0 || n > 200 {
		n = 20
	}
	return apply.Deploys(d.Config.DeploysFile(), n)
}

func (d *Daemon) logs(w http.ResponseWriter, r *http.Request) (any, error) {
	q := r.URL.Query()
	app, err := d.Applier.Live(q.Get("project"), q.Get("app"))
	if err != nil {
		return nil, apiError{http.StatusNotFound, err.Error()}
	}
	lines, _ := strconv.Atoi(q.Get("lines"))
	if lines <= 0 || lines > 5000 {
		lines = 200
	}
	container := app.Live()
	if q.Get("worker") != "" {
		container = app.Name + "-worker"
	}
	out, err := d.Machine.Logs(r.Context(), container, lines)
	return map[string]string{"container": container, "logs": out}, err
}

// refsOf are the release images of a project, as its deploys name them;
// all of them for "".
func (d *Daemon) refsOf(project string) ([]registry.Ref, error) {
	if project == "" {
		return d.Watch.Releases, nil
	}
	history, err := apply.Deploys(d.Config.DeploysFile(), 1000)
	if err != nil {
		return nil, err
	}
	for _, h := range history {
		if h.Project != project {
			continue
		}
		for _, ref := range d.Watch.Releases {
			if strings.HasPrefix(h.Release, ref.Host+"/"+ref.Repo+"@") {
				return []registry.Ref{ref}, nil
			}
		}
	}
	return nil, apiError{http.StatusNotFound, fmt.Sprintf("no release image of project %s has deployed here", project)}
}

type deployRequest struct {
	Project string `json:"project"`
	// Force deploys a release that was applied or failed before.
	Force bool `json:"force"`
}

func (d *Daemon) deploy(w http.ResponseWriter, r *http.Request) (any, error) {
	var req deployRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	refs, err := d.refsOf(req.Project)
	if err != nil {
		return nil, err
	}
	check := func(ctx context.Context, each func(*apply.Result)) error {
		for _, ref := range refs {
			res, err := d.Watch.Check(ctx, ref, req.Force)
			if err != nil {
				return fmt.Errorf("%s: %w", ref, err)
			}
			if res != nil {
				each(res)
			}
		}
		return nil
	}
	if r.Header.Get("Accept") == Streamed {
		return stream(w, r, func(ctx context.Context, send func(Event)) error {
			return check(ctx, func(res *apply.Result) { send(Event{Result: withLogs([]*apply.Result{res})[0]}) })
		})
	}
	var out []*apply.Result
	if err := check(r.Context(), func(res *apply.Result) { out = append(out, res) }); err != nil {
		return nil, err
	}
	return withLogs(out), nil
}

// withLogs puts the deploys' logs back in, which history leaves out.
func withLogs(rs []*apply.Result) []map[string]any {
	out := []map[string]any{}
	for _, r := range rs {
		b, _ := json.Marshal(r)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		m["log"], m["steps"] = r.Log, r.Steps
		out = append(out, m)
	}
	return out
}

func (d *Daemon) rollback(w http.ResponseWriter, r *http.Request) (any, error) {
	var req deployRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if req.Project == "" {
		return nil, badRequest("which project?")
	}
	refs, err := d.refsOf(req.Project)
	if err != nil {
		return nil, err
	}
	if r.Header.Get("Accept") == Streamed {
		return stream(w, r, func(ctx context.Context, send func(Event)) error {
			res, err := d.Watch.Rollback(ctx, refs[0])
			if err == nil {
				send(Event{Result: withLogs([]*apply.Result{res})[0]})
			}
			return err
		})
	}
	res, err := d.Watch.Rollback(r.Context(), refs[0])
	if err != nil {
		return nil, apiError{http.StatusConflict, err.Error()}
	}
	return withLogs([]*apply.Result{res})[0], nil
}

func (d *Daemon) backupNow(w http.ResponseWriter, r *http.Request) (any, error) {
	if d.Vault == nil {
		return nil, apiError{http.StatusConflict, "no backup bucket configured"}
	}
	// A backup outlives the request that asked for it.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Hour)
	defer cancel()
	return d.BackupAll(ctx), nil
}

func (d *Daemon) listBackups(w http.ResponseWriter, r *http.Request) (any, error) {
	if d.Vault == nil {
		return nil, apiError{http.StatusConflict, "no backup bucket configured"}
	}
	project := r.URL.Query().Get("project")
	if project == "" {
		return nil, badRequest("which project?")
	}
	return d.Vault.List(r.Context(), project)
}

type restoreRequest struct {
	Project string    `json:"project"`
	Name    string    `json:"name"` // "main", or "crm/uploads"
	At      time.Time `json:"at"`   // the backup's; zero is the newest
}

func (d *Daemon) restore(w http.ResponseWriter, r *http.Request) (any, error) {
	if d.Vault == nil {
		return nil, apiError{http.StatusConflict, "no backup bucket configured"}
	}
	var req restoreRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	all, err := d.Vault.List(r.Context(), req.Project)
	if err != nil {
		return nil, err
	}
	var m *backups.Manifest
	for i := range all {
		if all[i].Name == req.Name && (req.At.IsZero() || all[i].At.Equal(req.At)) {
			m = &all[i]
			break // newest first
		}
	}
	if m == nil {
		return nil, apiError{http.StatusNotFound, fmt.Sprintf("no backup of %s/%s like that", req.Project, req.Name)}
	}
	ctx := context.WithoutCancel(r.Context())
	var log strings.Builder
	switch m.Kind {
	case backups.Database:
		res, err := d.Applier.Resources()
		if err != nil {
			return nil, err
		}
		for _, x := range res {
			if x.Project == req.Project && x.Kind == "database" && x.Name == req.Name {
				fmt.Fprintln(&log, "restoring", m)
				return map[string]string{"log": log.String()}, d.Vault.RestoreDatabase(ctx, *m, x.DB)
			}
		}
		return nil, apiError{http.StatusNotFound, fmt.Sprintf("database %s/%s isn't on this server: deploy the project first", req.Project, req.Name)}
	default:
		app, vol, _ := strings.Cut(req.Name, "/")
		spec, err := d.Applier.Live(req.Project, app)
		if err != nil {
			return nil, apiError{http.StatusNotFound, err.Error()}
		}
		fmt.Fprintln(&log, "restoring", m)
		err = d.Vault.RestoreVolume(ctx, *m, spec, vol, &log)
		return map[string]string{"log": log.String()}, err
	}
}

type destroyRequest struct {
	Project string `json:"project"`
	Name    string `json:"name"`
	// Confirm repeats "project/name", so a slip of the CLI can't.
	Confirm string `json:"confirm"`
}

func (d *Daemon) destroy(w http.ResponseWriter, r *http.Request) (any, error) {
	var req destroyRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if req.Confirm != req.Project+"/"+req.Name {
		return nil, badRequest("confirm must repeat %s/%s", req.Project, req.Name)
	}
	if d.Vault == nil {
		return nil, apiError{http.StatusConflict, "no backup bucket configured, and nothing is destroyed without a last backup"}
	}
	var log strings.Builder
	ctx := context.WithoutCancel(r.Context())
	err := d.Applier.Destroy(ctx, d.Machine, req.Project, req.Name, func(x apply.Resource) error {
		if x.Kind == "cache" {
			return nil // nothing in it that can't be rebuilt
		}
		m, err := d.backup(ctx, x)
		if err == nil {
			fmt.Fprintln(&log, "last backup:", m)
		}
		return err
	}, &log)
	if err != nil {
		return nil, apiError{http.StatusConflict, err.Error()}
	}
	return map[string]string{"log": log.String()}, nil
}
