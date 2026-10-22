// Package scaffold is hakobu init: the files a repository needs to deploy
// with hakobu, for its agent as much as for CI.
package scaffold

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"

	"github.com/hakobudev/hakobu/internal/detect"
	"github.com/hakobudev/hakobu/internal/spec"
)

// Options are what init can't find in the repository.
type Options struct {
	Dir     string // the repository's root
	Server  string // the server it deploys to
	Image   string // the release image, without a tag ("ghcr.io/owner/repo")
	Branch  string // the branch that deploys
	Version string // this hakobu's, for the workflow to use the same
}

// File is one file init wrote or left alone.
type File struct {
	Path string
	Kept bool // it was there, so init left it as it was
}

//go:embed SKILL.md
var skill string

// Workflow is the reusable workflow a project's CI calls, in this
// repository.
const Workflow = "hakobudev/hakobu/.github/workflows/build.yml"

// Init writes hakobu.toml (with the apps it finds), the CI workflow, the
// skill and the Claude Code settings, leaving any file that's there; the
// skill is hakobu's, so it's always this version's.
func Init(o Options) ([]File, error) {
	if !namePattern.MatchString(o.Server) {
		return nil, fmt.Errorf("server %q isn't a server's name; hakobu server list shows them", o.Server)
	}
	if last := o.Image[strings.LastIndex(o.Image, "/")+1:]; !strings.Contains(o.Image, "/") || strings.ContainsAny(last, ":@") {
		return nil, fmt.Errorf("image %q: give the release image without its tag, like ghcr.io/owner/repo", o.Image)
	}
	if o.Branch == "" {
		o.Branch = "main"
	}
	toml, railpack, err := projectFile(o)
	if err != nil {
		return nil, err
	}
	var files []File
	write := func(name, text string, replace bool) error {
		p := filepath.Join(o.Dir, filepath.FromSlash(name))
		if _, err := os.Stat(p); err == nil && !replace {
			files = append(files, File{Path: name, Kept: true})
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		files = append(files, File{Path: name})
		return os.WriteFile(p, []byte(text), 0o644)
	}
	// A release's version is its tag without the v; a build of a branch
	// ("dev") uses the workflow of main.
	ref := "main"
	if v := "v" + strings.TrimPrefix(o.Version, "v"); releasePattern.MatchString(v) {
		ref = v
	}
	var wf bytes.Buffer
	if err := workflow.Execute(&wf, map[string]any{"Branch": o.Branch, "Uses": Workflow + "@" + ref, "Version": ref, "Image": o.Image, "Railpack": railpack}); err != nil {
		return nil, err
	}
	steps := []func() error{
		func() error { return write(spec.File, toml, false) },
		func() error { return write(".github/workflows/hakobu.yml", wf.String(), false) },
		func() error { return write(".claude/skills/hakobu/SKILL.md", skill, true) },
		func() error { return appendClaudeMD(o.Dir, &files) },
		func() error { return mergeJSON(o.Dir, ".claude/settings.json", settings, &files) },
		func() error { return mergeJSON(o.Dir, ".mcp.json", mcpServers, &files) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return files, err
		}
	}
	return files, nil
}

var (
	namePattern    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	releasePattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+`)
)

// projectFile is a hakobu.toml with an app for each directory that has a
// Dockerfile or a stack Railpack builds, checked like any other.
func projectFile(o Options) (text string, railpack bool, err error) {
	presets := detect.Scan(repoFiles(o.Dir), func(name string) string {
		b, _ := os.ReadFile(filepath.Join(o.Dir, filepath.FromSlash(name)))
		return string(b)
	})
	type app struct {
		Name, Path, Stack string
		Port              int
		Railpack          bool
	}
	var apps []app
	seen := map[string]bool{}
	for _, p := range presets {
		if seen[p.Path] { // a Dockerfile comes before Railpack's guess
			continue
		}
		seen[p.Path] = true
		name := "web"
		if p.Path != "." {
			name = toName(p.Path)
		}
		apps = append(apps, app{Name: name, Path: p.Path, Stack: p.Stack, Port: p.Port, Railpack: p.Strategy == "railpack"})
		railpack = railpack || p.Strategy == "railpack"
	}
	if len(apps) == 0 {
		apps = []app{{Name: "web", Path: "."}}
	}
	sort.SliceStable(apps, func(i, j int) bool { return apps[i].Path == "." && apps[j].Path != "." })
	var b strings.Builder
	fmt.Fprintf(&b, "# What %s runs for this repository; hakobu validate checks it.\n", o.Server)
	fmt.Fprintf(&b, "# Reference: https://hakobu.dev/docs/hakobu-toml\nproject = %q\nserver = %q\n", toName(filepath.Base(absDir(o.Dir))), o.Server)
	for _, a := range apps {
		fmt.Fprintf(&b, "\n[apps.%s]\n", a.Name)
		switch {
		case a.Stack != "" && a.Railpack:
			fmt.Fprintf(&b, "# %s, built by Railpack (no Dockerfile)\n", a.Stack)
		case a.Stack != "":
			fmt.Fprintf(&b, "# %s, built from its Dockerfile\n", a.Stack)
		}
		if a.Path != "." {
			fmt.Fprintf(&b, "path = %q\n", a.Path)
		}
		if a.Port != 0 {
			fmt.Fprintf(&b, "port = %d\n", a.Port)
		}
		b.WriteString("# domain = \"app.example.com\"\n# health = \"/up\"  # answers 2xx once it takes requests\n")
	}
	b.WriteString("\n# A database for an app: [databases.main], then uses = [\"main\"] in the app.\n")
	if _, err := spec.Parse([]byte(b.String())); err != nil {
		return "", false, fmt.Errorf("the %s init made is wrong (a bug): %w", spec.File, err)
	}
	return b.String(), railpack, nil
}

// repoFiles lists the root's files and those one directory down, as
// detect reads them, leaving out hidden and dependency directories.
func repoFiles(dir string) []string {
	var files []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		name := e.Name()
		switch {
		case e.Type().IsRegular():
			files = append(files, name)
		case e.IsDir() && !strings.HasPrefix(name, ".") && name != "node_modules" && name != "vendor":
			sub, _ := os.ReadDir(filepath.Join(dir, name))
			for _, s := range sub {
				if s.Type().IsRegular() {
					files = append(files, name+"/"+s.Name())
				}
			}
		}
	}
	return files
}

var notName = regexp.MustCompile(`[^a-z0-9-]+`)

// toName makes a directory's name one hakobu.toml takes.
func toName(s string) string {
	s = strings.Trim(notName.ReplaceAllString(strings.ToLower(s), "-"), "-")
	s = strings.TrimLeft(s, "0123456789-")
	if len(s) > 31 {
		s = strings.TrimRight(s[:31], "-")
	}
	if s == "" {
		return "app"
	}
	return s
}

func absDir(dir string) string {
	if a, err := filepath.Abs(dir); err == nil {
		return a
	}
	return dir
}

var workflow = template.Must(template.New("workflow").Parse(`# Builds each push to {{.Branch}} and hands it to the server, which
# deploys it; the job fails if the deploy does. Tests can go before it:
# a job of yours, and needs: [test] on this one.
name: deploy

on:
  push:
    branches: [{{.Branch}}]

jobs:
  deploy:
    permissions:
      contents: read
      packages: write  # push to ghcr.io
      statuses: read   # wait for the server's hakobu/deploy status
    uses: {{.Uses}}
    with:
      version: {{.Version}}
      image: {{.Image}}
      railpack: {{.Railpack}}
      # runner: ubuntu-24.04-arm  # for an arm64 server
`))

const claudeMarker = "<!-- hakobu -->"

const claudeSection = claudeMarker + `
## Deploys (hakobu)

This repository deploys with hakobu: hakobu.toml says what its server runs, and each push to the default branch deploys. Before changing hakobu.toml, migrations, the Dockerfile or secrets, read the hakobu skill (.claude/skills/hakobu/SKILL.md).

- Never run ` + "`hakobu destroy`" + ` or ` + "`hakobu restore`" + `, and never ask to: they're a person's to run.
- Secrets: never read, print or commit one. Ask the person to run ` + "`hakobu secret set APP NAME`" + `; hakobu secret list shows the names.
- Migrations stay backward compatible: the version running uses the database while the next one migrates it.
`

// appendClaudeMD adds hakobu's section to CLAUDE.md, once.
func appendClaudeMD(dir string, files *[]File) error {
	p := filepath.Join(dir, "CLAUDE.md")
	old, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if bytes.Contains(old, []byte(claudeMarker)) {
		*files = append(*files, File{Path: "CLAUDE.md", Kept: true})
		return nil
	}
	text := string(old)
	if text != "" {
		text = strings.TrimRight(text, "\n") + "\n\n"
	}
	*files = append(*files, File{Path: "CLAUDE.md"})
	return os.WriteFile(p, []byte(text+claudeSection), 0o644)
}

// settings keeps an agent off what's a person's to do, even if it's asked.
var settings = map[string]any{"permissions": map[string]any{"deny": []any{
	"Bash(hakobu destroy:*)",
	"Bash(hakobu restore:*)",
}}}

var mcpServers = map[string]any{"mcpServers": map[string]any{"hakobu": map[string]any{"command": "hakobu", "args": []any{"mcp"}}}}

// mergeJSON adds add's keys to a JSON file, keeping what's there: lists
// gain the entries they lack, objects the keys.
func mergeJSON(dir, name string, add map[string]any, files *[]File) error {
	p := filepath.Join(dir, filepath.FromSlash(name))
	doc := map[string]any{}
	old, err := os.ReadFile(p)
	switch {
	case err == nil:
		if err := json.Unmarshal(old, &doc); err != nil {
			return fmt.Errorf("%s: %w; not changing it", name, err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	if !merge(doc, add) {
		*files = append(*files, File{Path: name, Kept: true})
		return nil
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	*files = append(*files, File{Path: name})
	return os.WriteFile(p, append(b, '\n'), 0o644)
}

// merge reports whether it changed dst.
func merge(dst, src map[string]any) bool {
	changed := false
	for k, v := range src {
		cur, ok := dst[k]
		if !ok {
			dst[k], changed = v, true
			continue
		}
		switch v := v.(type) {
		case map[string]any:
			if m, ok := cur.(map[string]any); ok && merge(m, v) {
				changed = true
			}
		case []any:
			list, ok := cur.([]any)
			if !ok {
				continue
			}
			for _, e := range v {
				if !contains(list, e) {
					list, changed = append(list, e), true
				}
			}
			dst[k] = list
		}
	}
	return changed
}

func contains(list []any, e any) bool {
	for _, x := range list {
		if x == e {
			return true
		}
	}
	return false
}
