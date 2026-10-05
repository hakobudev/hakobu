package node

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
)

// Remote is a node on another server as the panel calls it, over the
// link: each call opens a stream, and the node's answer comes back as it
// goes (see Serve). The panel serves Handler on the link's streams the
// node opens, for the URLs of backups.
type Remote struct {
	client *http.Client
	mu     sync.Mutex
	urls   map[string]func(int) (string, error) // the callbacks of calls under way
}

var _ Node = (*Remote)(nil)

// ErrUnreachable: the call didn't get through to the node, or its answer
// broke off. The node may have done some of it.
var ErrUnreachable = errors.New("the server isn't reachable")

// NewRemote calls the node through streams from dial.
func NewRemote(dial func() (net.Conn, error)) *Remote {
	return &Remote{
		client: &http.Client{Transport: &http.Transport{
			DialContext:       func(context.Context, string, string) (net.Conn, error) { return dial() },
			DisableKeepAlives: true, // a stream per call: one cancelled ends only its own
		}},
		urls: map[string]func(int) (string, error){},
	}
}

// Handler answers the node's requests for the URLs of a backup's parts.
func (r *Remote) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /callback/{id}", func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		url := r.urls[req.PathValue("id")]
		r.mu.Unlock()
		part, err := strconv.Atoi(req.URL.Query().Get("part"))
		if url == nil || err != nil || part < 0 {
			http.Error(w, "no such backup", http.StatusNotFound)
			return
		}
		u, err := url(part)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, u)
	})
	return mux
}

// TellPanelURL tells the node the panel's new address ("https://host"),
// which it connects at from then on.
func (r *Remote) TellPanelURL(ctx context.Context, url string) error {
	body, _ := json.Marshal(map[string]string{"URL": url})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://node/panel", bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("the server didn't take the panel's address (%d): %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
	return nil
}

// remoteError is an error the node returned; it unwraps to the panel's
// sentinel for its code.
type remoteError struct {
	msg      string
	sentinel error
}

func (e remoteError) Error() string { return e.msg }
func (e remoteError) Unwrap() error { return e.sentinel }

// call calls method with args, passing output to out and events to
// onEvent, and decodes the results into results.
func (r *Remote) call(ctx context.Context, method string, out io.Writer, onEvent func(deathEvent), args []any, results ...any) error {
	var wire []any
	var ids []string
	defer func() {
		r.mu.Lock()
		for _, id := range ids {
			delete(r.urls, id)
		}
		r.mu.Unlock()
	}()
	for _, a := range args {
		switch v := a.(type) {
		case Upload:
			id := r.register(v.URL)
			ids = append(ids, id)
			wire = append(wire, wireUpload{v, id})
		case Download:
			id := r.register(v.URL)
			ids = append(ids, id)
			wire = append(wire, wireDownload{v, id})
		default:
			wire = append(wire, a)
		}
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://node/call/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("the server can't do %s (%d): %s", method, resp.StatusCode, msg)
	}
	fr, err := readFrames(resp.Body, out, onEvent)
	if err != nil {
		return fmt.Errorf("%w: the answer to %s broke off: %v", ErrUnreachable, method, err)
	}
	if fr.T == "error" {
		return remoteError{fr.M, errorCodes[fr.C]}
	}
	if len(fr.R) != len(results) {
		return fmt.Errorf("%s: %d results from the server, want %d", method, len(fr.R), len(results))
	}
	for i, res := range results {
		if err := json.Unmarshal(fr.R[i], res); err != nil {
			return fmt.Errorf("%s: %w", method, err)
		}
	}
	return nil
}

func (r *Remote) register(url func(int) (string, error)) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	r.mu.Lock()
	r.urls[id] = url
	r.mu.Unlock()
	return id
}

// bg is the context of the methods that take none.
func bg() context.Context { return context.Background() }

// Containers.

func (r *Remote) State(ctx context.Context, container string) State {
	var st State
	if err := r.call(ctx, "State", nil, nil, []any{container}, &st); err != nil {
		return State{Status: "unknown"}
	}
	return st
}

func (r *Remote) Statuses(ctx context.Context) (m map[string]string, err error) {
	err = r.call(ctx, "Statuses", nil, nil, nil, &m)
	return m, err
}

func (r *Remote) Logs(ctx context.Context, container string, lines int) (s string, err error) {
	err = r.call(ctx, "Logs", nil, nil, []any{container, lines}, &s)
	return s, err
}

// Builds.

func (r *Remote) Build(ctx context.Context, b BuildSpec, out io.Writer) (stack string, err error) {
	err = r.call(ctx, "Build", out, nil, []any{b}, &stack)
	return stack, err
}

func (r *Remote) ExposedPort(ctx context.Context, app string, img Image) (p int64) {
	_ = r.call(ctx, "ExposedPort", nil, nil, []any{app, img}, &p)
	return p
}

func (r *Remote) HasImage(ctx context.Context, app string, img Image) (ok bool, err error) {
	err = r.call(ctx, "HasImage", nil, nil, []any{app, img}, &ok)
	return ok, err
}

func (r *Remote) DropImage(ctx context.Context, app string, img Image) {
	_ = r.call(ctx, "DropImage", nil, nil, []any{app, img})
}

func (r *Remote) Promote(ctx context.Context, app string) error {
	return r.call(ctx, "Promote", nil, nil, []any{app})
}

func (r *Remote) SwapForRollback(ctx context.Context, app string) error {
	return r.call(ctx, "SwapForRollback", nil, nil, []any{app})
}

// Rolling out.

func (r *Remote) StartCandidate(ctx context.Context, app AppSpec, img Image, out io.Writer) (c Candidate, err error) {
	err = r.call(ctx, "StartCandidate", out, nil, []any{app, img}, &c)
	return c, err
}

func (r *Remote) Switch(ctx context.Context, app AppSpec, c Candidate) error {
	return r.call(ctx, "Switch", nil, nil, []any{app, c})
}

func (r *Remote) Retire(ctx context.Context, app AppSpec, c Candidate, out io.Writer) {
	if err := r.call(ctx, "Retire", out, nil, []any{app, c}); err != nil {
		fmt.Fprintln(out, "warning: removing the previous version:", err)
	}
}

// Discard returns err, the reason the candidate goes, whatever becomes of
// the call; the node gets no error to return.
func (r *Remote) Discard(ctx context.Context, app AppSpec, c Candidate, out io.Writer, err error) error {
	if cerr := r.call(ctx, "Discard", out, nil, []any{app, c, nil}); cerr != nil {
		fmt.Fprintln(out, "warning: removing the new version:", cerr)
	}
	return err
}

// The live version.

func (r *Remote) StopApp(ctx context.Context, app AppSpec) error {
	return r.call(ctx, "StopApp", nil, nil, []any{app})
}

func (r *Remote) StartApp(ctx context.Context, app AppSpec, out io.Writer) {
	if err := r.call(ctx, "StartApp", out, nil, []any{app}); err != nil {
		fmt.Fprintln(out, "failed to start the app again:", err)
	}
}

func (r *Remote) EnsureProxy(ctx context.Context, app AppSpec) {
	_ = r.call(ctx, "EnsureProxy", nil, nil, []any{app})
}

func (r *Remote) RemoveProxy(app string) {
	_ = r.call(bg(), "RemoveProxy", nil, nil, []any{app})
}

func (r *Remote) Reconcile(ctx context.Context, apps []AppSpec) {
	_ = r.call(ctx, "Reconcile", nil, nil, []any{apps})
}

// Workers.

func (r *Remote) RunWorker(ctx context.Context, w WorkerSpec, out io.Writer) error {
	return r.call(ctx, "RunWorker", out, nil, []any{w})
}

func (r *Remote) RemoveWorker(ctx context.Context, app string) error {
	return r.call(ctx, "RemoveWorker", nil, nil, []any{app})
}

func (r *Remote) RemoveApp(ctx context.Context, app string, volumes []string) error {
	return r.call(ctx, "RemoveApp", nil, nil, []any{app, volumes})
}

// Databases.

func (r *Remote) EnsurePostgres(ctx context.Context) error {
	return r.call(ctx, "EnsurePostgres", nil, nil, nil)
}

func (r *Remote) PostgresReady(ctx context.Context) (ok bool) {
	_ = r.call(ctx, "PostgresReady", nil, nil, nil, &ok)
	return ok
}

func (r *Remote) CreateDatabase(ctx context.Context, d DBSpec) error {
	return r.call(ctx, "CreateDatabase", nil, nil, []any{d})
}

func (r *Remote) EnsureDatabase(ctx context.Context, d DBSpec) error {
	return r.call(ctx, "EnsureDatabase", nil, nil, []any{d})
}

func (r *Remote) DropDatabase(ctx context.Context, d DBSpec) error {
	return r.call(ctx, "DropDatabase", nil, nil, []any{d})
}

func (r *Remote) SetPassword(ctx context.Context, d DBSpec) error {
	return r.call(ctx, "SetPassword", nil, nil, []any{d})
}

// Backups.

func (r *Remote) BackupDatabase(ctx context.Context, d DBSpec, up Upload) (u Uploaded, err error) {
	err = r.call(ctx, "BackupDatabase", nil, nil, []any{d, up}, &u)
	return u, err
}

func (r *Remote) RestoreDatabase(ctx context.Context, d DBSpec, dl Download) error {
	return r.call(ctx, "RestoreDatabase", nil, nil, []any{d, dl})
}

func (r *Remote) VerifyDatabaseBackup(ctx context.Context, d DBSpec, dl Download) (tables int, err error) {
	err = r.call(ctx, "VerifyDatabaseBackup", nil, nil, []any{d, dl}, &tables)
	return tables, err
}

func (r *Remote) BackupVolume(ctx context.Context, app, name string, up Upload) (u Uploaded, err error) {
	err = r.call(ctx, "BackupVolume", nil, nil, []any{app, name, up}, &u)
	return u, err
}

func (r *Remote) RestoreVolume(ctx context.Context, app AppSpec, name string, dl Download, out io.Writer) error {
	return r.call(ctx, "RestoreVolume", out, nil, []any{app, name, dl})
}

func (r *Remote) VerifyVolumeBackup(ctx context.Context, dl Download) (files int, err error) {
	err = r.call(ctx, "VerifyVolumeBackup", nil, nil, []any{dl}, &files)
	return files, err
}

// Snapshots.

func (r *Remote) SaveDump(ctx context.Context, d DBSpec) (dump Dump, err error) {
	err = r.call(ctx, "SaveDump", nil, nil, []any{d}, &dump)
	return dump, err
}

func (r *Remote) DropDump(dump Dump) {
	_ = r.call(bg(), "DropDump", nil, nil, []any{dump})
}

func (r *Remote) KeepSnapshot(app string, dump Dump) error {
	return r.call(bg(), "KeepSnapshot", nil, nil, []any{app, dump})
}

func (r *Remote) HasSnapshot(app string) (ok bool) {
	_ = r.call(bg(), "HasSnapshot", nil, nil, []any{app}, &ok)
	return ok
}

func (r *Remote) SetAside(app string, dump Dump) (where string) {
	if err := r.call(bg(), "SetAside", nil, nil, []any{app, dump}, &where); err != nil {
		return string(dump) + " on the server"
	}
	return where
}

func (r *Remote) PruneSnapshots(apps map[string]bool) {
	_ = r.call(bg(), "PruneSnapshots", nil, nil, []any{apps})
}

func (r *Remote) ReplaceDatabase(ctx context.Context, d DBSpec, dump Dump) error {
	return r.call(ctx, "ReplaceDatabase", nil, nil, []any{d, dump})
}

// Volumes.

func (r *Remote) HasVolume(ctx context.Context, app, name string) (ok bool, err error) {
	err = r.call(ctx, "HasVolume", nil, nil, []any{app, name}, &ok)
	return ok, err
}

func (r *Remote) RemoveVolume(ctx context.Context, app, name string) error {
	return r.call(ctx, "RemoveVolume", nil, nil, []any{app, name})
}

func (r *Remote) RemoveVolumesExcept(ctx context.Context, keep map[string]bool) error {
	return r.call(ctx, "RemoveVolumesExcept", nil, nil, []any{keep})
}

// The machine.

func (r *Remote) Readings(ctx context.Context, containers map[string]bool) (rd Readings, err error) {
	err = r.call(ctx, "Readings", nil, nil, []any{containers}, &rd)
	return rd, err
}

func (r *Remote) Disk(ctx context.Context) (used, total uint64, err error) {
	err = r.call(ctx, "Disk", nil, nil, nil, &used, &total)
	return used, total, err
}

func (r *Remote) Cleanup(ctx context.Context, c CleanupSpec) (freed int64, images int, err error) {
	err = r.call(ctx, "Cleanup", nil, nil, []any{c}, &freed, &images)
	return freed, images, err
}

func (r *Remote) WatchDeaths(ctx context.Context, fn func(container, app string, d Death)) error {
	return r.call(ctx, "WatchDeaths", nil, func(e deathEvent) { fn(e.Container, e.App, e.Death) }, nil)
}

func (r *Remote) CheckHealth(ctx context.Context, app AppSpec, port int64) (ok bool, why string, checked bool) {
	if err := r.call(ctx, "CheckHealth", nil, nil, []any{app, port}, &ok, &why, &checked); err != nil {
		return false, "", false
	}
	return ok, why, checked
}

// Networks.

func (r *Remote) EnsureProjectNetworks(ctx context.Context, project string, tunnels []TunnelSpec) error {
	return r.call(ctx, "EnsureProjectNetworks", nil, nil, []any{project, tunnels})
}

func (r *Remote) RemoveProjectNetworks(ctx context.Context, project string, tunnels []TunnelSpec) error {
	return r.call(ctx, "RemoveProjectNetworks", nil, nil, []any{project, tunnels})
}

func (r *Remote) RunTunnel(ctx context.Context, t TunnelSpec) error {
	return r.call(ctx, "RunTunnel", nil, nil, []any{t})
}

func (r *Remote) RemoveTunnel(ctx context.Context, client string) error {
	return r.call(ctx, "RemoveTunnel", nil, nil, []any{client})
}

func (r *Remote) StartIngestRelay(ctx context.Context, socket string) error {
	return r.call(ctx, "StartIngestRelay", nil, nil, []any{socket})
}
