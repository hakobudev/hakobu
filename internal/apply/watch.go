package apply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"filippo.io/age"
	"github.com/hakobudev/hakobu/internal/registry"
	"github.com/hakobudev/hakobu/internal/seal"
	"github.com/hakobudev/hakobu/internal/spec"
	"github.com/hakobudev/hakobu/internal/status"
)

// A release image is how CI hands a commit to its server: FROM scratch,
// with the project's hakobu.toml, its sealed secrets and the image of each
// app (pinned by digest) in labels. The server polls its tag, reads the
// labels from the registry and deploys every app of the commit together.
const (
	LabelSpec     = "dev.hakobu.spec"
	LabelSecrets  = "dev.hakobu.secrets"
	LabelImages   = "dev.hakobu.images"
	LabelSource   = "org.opencontainers.image.source"
	LabelRevision = "org.opencontainers.image.revision"
)

// Meta is where a release came from.
type Meta struct {
	Ref      string // the release image, pinned
	Source   string // the repository's URL
	Revision string // the commit
	// StatusToken may set the commit's status (from the project's sealed
	// [hakobu] secrets); it's never logged.
	StatusToken string
}

// ReadRelease reads a release image's labels, opening its secrets with
// the server's key.
func ReadRelease(labels map[string]string, id age.Identity) (Release, Meta, error) {
	meta := Meta{Source: labels[LabelSource], Revision: labels[LabelRevision]}
	text, ok := labels[LabelSpec]
	if !ok {
		return Release{}, meta, fmt.Errorf("not a release image: no %s label", LabelSpec)
	}
	p, err := spec.Parse([]byte(text))
	if err != nil {
		return Release{}, meta, fmt.Errorf("hakobu.toml: %w", err)
	}
	var images map[string]string
	if err := json.Unmarshal([]byte(labels[LabelImages]), &images); err != nil {
		return Release{}, meta, fmt.Errorf("%s: %w", LabelImages, err)
	}
	var errs []error
	for _, app := range sortedKeys(p.Apps) {
		ref, ok := images[app]
		if !ok {
			errs = append(errs, fmt.Errorf("apps.%s: the release has no image for it", app))
			continue
		}
		if r, err := registry.ParseRef(ref); err != nil || r.Digest == "" {
			errs = append(errs, fmt.Errorf("apps.%s: %q should be an image pinned by digest (…@sha256:…)", app, ref))
		}
	}
	for _, app := range sortedKeys(images) {
		if _, ok := p.Apps[app]; !ok {
			errs = append(errs, fmt.Errorf("the release has an image for %s, which hakobu.toml doesn't have", app))
		}
	}
	r := Release{Spec: p, Images: images, Revision: meta.Revision}
	if sealed := strings.TrimSpace(labels[LabelSecrets]); sealed != "" {
		secrets, err := openSecrets(sealed, id, p)
		if err != nil {
			errs = append(errs, err)
		}
		r.Secrets, meta.StatusToken = secrets.Apps, secrets.Hakobu[seal.StatusToken]
	}
	if _, err := status.Repo(meta.Source); err == nil && meta.StatusToken == "" {
		errs = append(errs, fmt.Errorf("%s: no token for the commit status; add one that may set commit statuses on the repository with hakobu secret set hakobu %s", seal.FileName, seal.StatusToken))
	}
	return r, meta, errors.Join(errs...)
}

// openSecrets opens hakobu.secrets.toml and checks it against the spec: a
// secret of no app, or one that a variable from the file already sets,
// is a mistake to stop on.
func openSecrets(text string, id age.Identity, p *spec.Project) (seal.Opened, error) {
	s, err := seal.Parse([]byte(text))
	if err != nil {
		return seal.Opened{}, fmt.Errorf("%s: %w", seal.FileName, err)
	}
	if id == nil {
		return seal.Opened{}, fmt.Errorf("%s: this server has no key to open it", seal.FileName)
	}
	o, err := s.Open(id)
	if err != nil {
		return o, fmt.Errorf("%s: %w", seal.FileName, err)
	}
	var errs []error
	for _, app := range sortedKeys(o.Apps) {
		a, ok := p.Apps[app]
		if !ok {
			errs = append(errs, fmt.Errorf("%s: apps.%s: hakobu.toml has no such app", seal.FileName, app))
			continue
		}
		taken := map[string]bool{"PORT": true, spec.RevisionVar: true}
		for k := range a.Env {
			taken[k] = true
		}
		for res, prefix := range a.Uses {
			kind := "cache"
			if _, ok := p.Databases[res]; ok {
				kind = "database"
			}
			for _, v := range spec.Vars(kind, prefix) {
				taken[v] = true
			}
		}
		for _, k := range sortedKeys(o.Apps[app]) {
			if taken[k] {
				errs = append(errs, fmt.Errorf("%s: apps.%s.%s: hakobu.toml sets it already", seal.FileName, app, k))
			}
		}
	}
	return o, errors.Join(errs...)
}

// Result is one deploy, as deploys.jsonl keeps it.
type Result struct {
	At       time.Time `json:"at"`
	Project  string    `json:"project,omitempty"`
	Release  string    `json:"release"` // the release image, pinned
	Source   string    `json:"source,omitempty"`
	Revision string    `json:"revision,omitempty"`
	Changes  []string  `json:"changes,omitempty"`
	Error    string    `json:"error,omitempty"`
	Log      string    `json:"-"`
}

// Watch polls the release images of the server's projects and deploys
// each new one. A release that failed isn't tried again until a newer one
// comes, or someone asks (Check with force).
type Watch struct {
	Applier  *Applier
	Registry *registry.Client
	Releases []registry.Ref
	// Every is the time between polls (default 30 s).
	Every time.Duration
	// Identity is the server's key, which opens the releases' secrets.
	Identity age.Identity
	// WatchFile keeps the last release applied and failed of each ref;
	// DeploysFile gets a line for every deploy.
	WatchFile, DeploysFile string
	// Started and Done are told of each deploy, for commit statuses and
	// emails.
	Started func(Meta)
	Done    func(Meta, Result)
	Log     io.Writer

	mu sync.Mutex
}

type watched struct {
	// Digests of the release image: live now, live before it (the
	// rollback target), and the last that failed or was rolled back,
	// which polls skip until a newer one comes.
	Applied  string `json:"applied,omitempty"`
	Previous string `json:"previous,omitempty"`
	Failed   string `json:"failed,omitempty"`
}

// Run polls until ctx ends.
func (w *Watch) Run(ctx context.Context) {
	every := w.Every
	if every == 0 {
		every = 30 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		for _, ref := range w.Releases {
			if _, err := w.Check(ctx, ref, false); err != nil && ctx.Err() == nil {
				fmt.Fprintln(w.log(), ref, err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Check deploys ref's release if it's new; force deploys it even if it
// was applied or failed before. It returns the deploy, nil if there was
// none, and an error only when it couldn't get as far as a deploy (the
// registry unreachable, say); a failed deploy is in the result.
func (w *Watch) Check(ctx context.Context, ref registry.Ref, force bool) (*Result, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	digest, err := w.Registry.Digest(ctx, ref)
	if err != nil {
		return nil, err
	}
	seen, err := w.load()
	if err != nil {
		return nil, err
	}
	key := ref.String()
	s := seen[key]
	if !force && (digest == s.Applied || digest == s.Failed) {
		return nil, nil
	}
	pinned := ref.Pinned(digest).String()
	res := Result{At: time.Now().UTC(), Release: pinned}
	var log strings.Builder
	release, meta, err := w.fetch(ctx, ref.Pinned(digest))
	meta.Ref = pinned
	res.Source, res.Revision = meta.Source, meta.Revision
	if err == nil {
		release.Force = force
		res.Project = release.Spec.Name
		if w.Started != nil {
			w.Started(meta)
		}
		var plan Plan
		plan, err = w.Applier.Apply(ctx, release, &log)
		for _, c := range plan.Changes {
			res.Changes = append(res.Changes, c.String())
		}
		if ctx.Err() != nil {
			// hakobu is stopping: not a failed release, which polls would
			// skip, but one to deploy again as hakobu starts.
			fmt.Fprintln(w.log(), pinned, "deploy cut short by hakobu stopping; it's tried again as hakobu starts")
			return nil, ctx.Err()
		}
	}
	res.Log = log.String()
	if err != nil {
		res.Error = err.Error()
		s.Failed = digest
	} else if digest != s.Applied {
		s.Previous, s.Applied, s.Failed = s.Applied, digest, ""
	}
	seen[key] = s
	return w.finish(seen, meta, res)
}

// Rollback deploys the release that was live before the current one, and
// holds the current one back: polls skip it until a newer release comes.
func (w *Watch) Rollback(ctx context.Context, ref registry.Ref) (*Result, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	seen, err := w.load()
	if err != nil {
		return nil, err
	}
	key := ref.String()
	s := seen[key]
	if s.Previous == "" {
		return nil, fmt.Errorf("%s: no earlier release to roll back to", ref)
	}
	pinned := ref.Pinned(s.Previous).String()
	res := Result{At: time.Now().UTC(), Release: pinned}
	var log strings.Builder
	release, meta, err := w.fetch(ctx, ref.Pinned(s.Previous))
	if err == nil && s.Applied != "" {
		// The code goes back; the secrets stay current, so a key replaced
		// since doesn't come back with it.
		current, cmeta, cerr := w.fetch(ctx, ref.Pinned(s.Applied))
		if cerr != nil {
			err = fmt.Errorf("reading the current release for its secrets: %w", cerr)
		}
		release.Secrets, meta.StatusToken = current.Secrets, cmeta.StatusToken
	}
	meta.Ref = pinned
	res.Source, res.Revision = meta.Source, meta.Revision
	if err == nil {
		res.Project = release.Spec.Name
		fmt.Fprintln(&log, "rolling back to", pinned)
		var plan Plan
		plan, err = w.Applier.Apply(ctx, release, &log)
		for _, c := range plan.Changes {
			res.Changes = append(res.Changes, c.String())
		}
	}
	res.Log = log.String()
	if err != nil {
		res.Error = "rollback: " + err.Error()
	} else {
		s.Failed, s.Applied, s.Previous = s.Applied, s.Previous, ""
	}
	seen[key] = s
	return w.finish(seen, meta, res)
}

func (w *Watch) finish(seen map[string]watched, meta Meta, res Result) (*Result, error) {
	if err := w.save(seen); err != nil {
		fmt.Fprintln(w.log(), "warning:", err)
	}
	w.record(res)
	if w.Done != nil {
		w.Done(meta, res)
	}
	return &res, nil
}

// fetch reads a release's labels: from the server's copy if it has one,
// as of every release it applied, else from the registry, keeping a
// copy. A rollback then needs no registry, nor its old releases, which a
// cleanup policy may have deleted.
func (w *Watch) fetch(ctx context.Context, ref registry.Ref) (Release, Meta, error) {
	file := w.releaseFile(ref.Digest)
	var labels map[string]string
	if b, err := os.ReadFile(file); err == nil && json.Unmarshal(b, &labels) == nil {
		return ReadRelease(labels, w.Identity)
	}
	labels, err := w.Registry.Labels(ctx, ref)
	if err != nil {
		return Release{}, Meta{}, err
	}
	if file != "" {
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err == nil {
			b, _ := json.Marshal(labels)
			_ = os.WriteFile(file, b, 0o600) // sealed secrets only, as in the registry
		}
	}
	return ReadRelease(labels, w.Identity)
}

// releaseFile keeps a release's labels by its digest, next to watch.json.
func (w *Watch) releaseFile(digest string) string {
	if digest == "" || w.WatchFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(w.WatchFile), "releases", strings.TrimPrefix(digest, "sha256:")+".json")
}

func (w *Watch) load() (map[string]watched, error) {
	seen := map[string]watched{}
	b, err := os.ReadFile(w.WatchFile)
	if errors.Is(err, os.ErrNotExist) {
		return seen, nil
	}
	if err != nil {
		return nil, err
	}
	return seen, json.Unmarshal(b, &seen)
}

func (w *Watch) save(seen map[string]watched) error {
	b, err := json.MarshalIndent(seen, "", "  ")
	if err != nil {
		return err
	}
	tmp := w.WatchFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, w.WatchFile)
}

// record appends the deploy to deploys.jsonl, the server's history.
func (w *Watch) record(r Result) {
	if w.DeploysFile == "" {
		return
	}
	b, _ := json.Marshal(r)
	f, err := os.OpenFile(w.DeploysFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		_, err = f.Write(append(b, '\n'))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		fmt.Fprintln(w.log(), "warning: deploys.jsonl:", err)
	}
}

// Deploys are the last n deploys from deploys.jsonl, newest first.
func Deploys(file string, n int) ([]Result, error) {
	b, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Result
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	for _, l := range slices.Backward(lines) {
		if len(out) == n {
			break
		}
		var r Result
		if json.Unmarshal([]byte(l), &r) == nil {
			out = append(out, r)
		}
	}
	return out, nil
}

func (w *Watch) log() io.Writer {
	if w.Log != nil {
		return w.Log
	}
	return io.Discard
}

// ReportTo has each deploy set its commit's status through r: pending as
// it starts, then success or failure with the reason. A release from
// outside GitHub has no status to set.
func (w *Watch) ReportTo(r status.Reporter) {
	report := func(m Meta, s status.State, desc string) {
		if m.Source == "" || m.Revision == "" {
			return
		}
		err := r.Report(context.Background(), status.Commit{Source: m.Source, Revision: m.Revision, Token: m.StatusToken}, s, desc)
		if err != nil && !errors.Is(err, status.ErrNotGitHub) {
			fmt.Fprintln(w.log(), "warning: commit status:", err)
		}
	}
	w.Started = func(m Meta) { report(m, status.Pending, "deploying") }
	w.Done = func(m Meta, res Result) {
		switch {
		case res.Error != "":
			report(m, status.Failure, res.Error)
		case len(res.Changes) == 0:
			report(m, status.Success, "live, nothing changed")
		default:
			report(m, status.Success, fmt.Sprintf("live: %d changes", len(res.Changes)))
		}
	}
}
