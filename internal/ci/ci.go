// Package ci is what a project's CI runs: it builds each app of the
// commit, pushes them, then pushes the release image a server polls for,
// and waits for the server to say how the deploy went.
package ci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/hakobudev/hakobu/internal/apply"
	"github.com/hakobudev/hakobu/internal/seal"
	"github.com/hakobudev/hakobu/internal/spec"
	"github.com/hakobudev/hakobu/internal/status"
)

// Runner runs a command in dir, its output going to the build's log.
type Runner func(ctx context.Context, dir string, log io.Writer, name string, args ...string) error

// Exec runs commands on this machine.
func Exec(ctx context.Context, dir string, log io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, log, log
	return cmd.Run()
}

// Build is one commit's release.
type Build struct {
	Dir      string // the repository's root
	Image    string // the release image, without a tag; app NAME goes to Image-NAME
	Tag      string // the tag the server polls, "main"
	Source   string // the repository's URL
	Revision string // the commit's full SHA
	// Pull is CI's own token for the apps' registry, which lives as long
	// as the job does (GitHub Actions' GITHUB_TOKEN): sealed to the
	// server in the release, it pulls the apps while CI waits for the
	// deploy, and the server keeps no token for the registry.
	Pull *apply.PullAuth
	Run  Runner
	Log  io.Writer
}

// Release builds and pushes each app from the Dockerfile in its
// directory, then the release image: FROM scratch, carrying
// hakobu.toml, the sealed secrets and each app's image pinned by digest
// in labels.
// The release goes last, so a server never sees one whose apps aren't
// pushed yet. It returns the release, pinned.
func (b Build) Release(ctx context.Context) (string, error) {
	text, err := os.ReadFile(filepath.Join(b.Dir, spec.File))
	if err != nil {
		return "", err
	}
	p, err := spec.Parse(text)
	if err != nil {
		return "", fmt.Errorf("%s: %w", spec.File, err)
	}
	secrets, err := os.ReadFile(filepath.Join(b.Dir, seal.FileName))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	var sealer *seal.Secrets
	if len(secrets) > 0 {
		s, err := seal.Parse(secrets)
		if err == nil {
			err = s.CheckApps(slices.Sorted(maps.Keys(p.Apps)))
		}
		if err != nil {
			return "", fmt.Errorf("%s: %w", seal.FileName, err)
		}
		sealer = s
	}
	if b.Image == "" || b.Tag == "" || b.Revision == "" {
		return "", errors.New("a build needs the image, its tag and the commit")
	}
	tmp, err := os.MkdirTemp("", "hakobu-build-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)

	names := make([]string, 0, len(p.Apps))
	for name := range p.Apps {
		names = append(names, name)
	}
	sort.Strings(names)
	// Every app's Dockerfile before any build, so one missing doesn't
	// show only after the others have built.
	var missing []error
	for _, name := range names {
		if p.Apps[name].Image != "" {
			continue
		}
		if _, err := os.Stat(dockerfile(b.Dir, p.Apps[name])); err != nil {
			missing = append(missing, fmt.Errorf("apps.%s: no Dockerfile in %s: hakobu builds each app from its own", name, path.Join(".", p.Apps[name].Path)))
		}
	}
	if err := errors.Join(missing...); err != nil {
		return "", err
	}
	images := map[string]string{}
	for _, name := range names {
		ref, err := b.app(ctx, tmp, name, p.Apps[name])
		if err != nil {
			return "", fmt.Errorf("apps.%s: %w", name, err)
		}
		images[name] = ref
	}

	imagesJSON, _ := json.Marshal(images)
	// With the secrets' recipients, hakobu.toml is sealed too, as is CI's
	// pull token: the release then reads nothing to whoever pulls it, and
	// may be public, which the server polls with no token at all.
	specLabel, specText := apply.LabelSpec, string(text)
	var extra []string
	if sealer != nil {
		if specText, err = sealer.Seal(string(text)); err != nil {
			return "", fmt.Errorf("sealing %s: %w", spec.File, err)
		}
		specLabel = apply.LabelSpecSealed
		if b.Pull != nil {
			pj, _ := json.Marshal(b.Pull)
			sealed, err := sealer.SealAt(seal.Place(p.Name, "hakobu", apply.PullPlace), string(pj))
			if err != nil {
				return "", fmt.Errorf("sealing the pull token: %w", err)
			}
			extra = append(extra, "--label", apply.LabelPull+"="+sealed)
		}
	} else if b.Pull != nil {
		fmt.Fprintf(b.log(), "warning: no %s to seal CI's pull token to: the server needs its own credentials for %s\n", seal.FileName, b.Pull.Host)
	}
	// One layer: a registry may refuse an image of none.
	if err := os.WriteFile(filepath.Join(tmp, "release"), []byte(specText), 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(tmp, "Dockerfile"), []byte("FROM scratch\nCOPY release /\n"), 0o644); err != nil {
		return "", err
	}
	args := []string{"--label", specLabel + "=" + specText, "--label", apply.LabelImages + "=" + string(imagesJSON)}
	if len(secrets) > 0 {
		args = append(args, "--label", apply.LabelSecrets+"="+string(secrets))
	}
	args = append(args, extra...)
	fmt.Fprintf(b.log(), "release → %s:%s\n", b.Image, b.Tag)
	digest, err := b.push(ctx, tmp, tmp, "release", b.Image+":"+b.Tag, args...)
	if err != nil {
		return "", fmt.Errorf("release: %w", err)
	}
	return b.Image + "@" + digest, nil
}

// app builds one app and returns its image, pinned. An app of a ready
// image is pinned to the digest its tag names now.
func (b Build) app(ctx context.Context, tmp, name string, a spec.App) (string, error) {
	if a.Image != "" {
		return b.pin(ctx, tmp, a.Image)
	}
	repo := b.Image + "-" + name
	fmt.Fprintf(b.log(), "%s → %s\n", name, repo)
	digest, err := b.push(ctx, filepath.Dir(dockerfile(b.Dir, a)), tmp, name, repo+":"+b.Revision)
	return repo + "@" + digest, err
}

// pin is ref by the digest of what it names now: the server runs that,
// whatever the tag names later.
func (b Build) pin(ctx context.Context, tmp, ref string) (string, error) {
	fmt.Fprintf(b.log(), "%s, as it is\n", ref)
	if strings.Contains(ref, "@") {
		return ref, nil
	}
	var out bytes.Buffer
	if err := b.Run(ctx, tmp, &out, "docker", "buildx", "imagetools", "inspect", ref, "--format", "{{.Manifest.Digest}}"); err != nil {
		return "", fmt.Errorf("%s: %w: %s", ref, err, strings.TrimSpace(out.String()))
	}
	digest := ""
	for _, line := range strings.Fields(out.String()) {
		if strings.HasPrefix(line, "sha256:") {
			digest = line
		}
	}
	if digest == "" {
		return "", fmt.Errorf("%s: no digest in %q", ref, out.String())
	}
	return strings.TrimSuffix(ref, ":"+spec.ImageTag(ref)) + "@" + digest, nil
}

func dockerfile(root string, a spec.App) string {
	return filepath.Join(root, filepath.FromSlash(a.Path), "Dockerfile")
}

// push builds dir with buildx and pushes it as tag, returning its digest.
func (b Build) push(ctx context.Context, dir, tmp, name, tag string, extra ...string) (string, error) {
	meta := filepath.Join(tmp, name+".metadata.json")
	args := []string{"buildx", "build", "--push", "--provenance=false", "-t", tag, "--metadata-file", meta,
		"--label", apply.LabelRevision + "=" + b.Revision}
	if b.Source != "" {
		args = append(args, "--label", apply.LabelSource+"="+b.Source)
	}
	args = append(append(args, extra...), ".")
	if err := b.Run(ctx, dir, b.log(), "docker", args...); err != nil {
		return "", err
	}
	raw, err := os.ReadFile(meta)
	if err != nil {
		return "", fmt.Errorf("buildx wrote no metadata: %w", err)
	}
	var m struct {
		Digest string `json:"containerimage.digest"`
	}
	if err := json.Unmarshal(raw, &m); err != nil || m.Digest == "" {
		return "", fmt.Errorf("no digest in buildx's metadata: %s", raw)
	}
	return m.Digest, nil
}

func (b Build) log() io.Writer {
	if b.Log != nil {
		return b.Log
	}
	return io.Discard
}

// Statuses reads a commit's hakobu/deploy status.
type Statuses interface {
	Get(ctx context.Context, c status.Commit) (state status.State, description string, ok bool, err error)
}

// Wait waits up to timeout for the server to deploy the commit, polling
// its status every few seconds: nil once it's live, the server's reason
// once it failed.
func Wait(ctx context.Context, s Statuses, c status.Commit, timeout, every time.Duration, log io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var last status.State
	for {
		state, desc, ok, err := s.Get(ctx, c)
		switch {
		case err != nil && ctx.Err() == nil:
			fmt.Fprintln(log, "warning:", err)
		case ok && state != last:
			fmt.Fprintf(log, "%s: %s\n", state, desc)
			last = state
		}
		switch {
		case ok && state == status.Success:
			return nil
		case ok && state != status.Pending:
			return fmt.Errorf("the deploy failed: %s (hakobu deploys, hakobu logs say more)", desc)
		}
		select {
		case <-ctx.Done():
			if last == status.Pending {
				return fmt.Errorf("still deploying after %s; hakobu deploys shows how it ends", timeout)
			}
			return fmt.Errorf("no %s status after %s: does the server poll this image (releases in its config), may it pull it (registries), and is the status token in hakobu.secrets.toml? hakobu deploy checks now and says what's wrong", status.Context, timeout)
		case <-time.After(every):
		}
	}
}
