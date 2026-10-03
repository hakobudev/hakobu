package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/a-h/templ"
	"github.com/x0ryz/hakobu/internal/store"
)

// The panel's pages are templ components (ui_*.templ, generated into
// *_templ.go by `go generate ./cmd`). ui_kit.templ holds the design system:
// pages build from its components and the classes in styles.css.

// renderPage writes a component as the response.
func renderPage(w http.ResponseWriter, r *http.Request, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := c.Render(r.Context(), w); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// ic draws an icon of the sprite as a component.
func ic(name string, classes ...string) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		h, err := icon(name, classes...)
		if err != nil {
			return err
		}
		_, err = io.WriteString(w, string(h))
		return err
	})
}

// crumb is a step of the context in the top bar: project, then app.
type crumb struct {
	Label, Href, Icon string
	Logo              string // a logo instead of the icon
	Switch            string // where to load a menu of its siblings from
}

// tab is a link in a page's tab bar; Count shows a counter after it.
type tab struct {
	Label, Icon, Href string
	Current           bool
	Count             int
	Danger            bool // the counter is of problems
}

// pageHead is the top of a page: icon, title, status, a line of facts and
// the page's main actions.
type pageHead struct {
	Icon, Title           string
	Logo                  string // drawn instead of Icon; neither for no tile
	Mono                  bool
	Status, Meta, Actions templ.Component
}

// segment is a choice of a segmented control: a link, or with HX an htmx
// request that replaces the closest Target.
type segment struct {
	Label, Href string
	Current     bool
	HX          bool
	Target      string
}

func pageTitle(title string) string {
	if title == "" {
		return "Hakobu"
	}
	return title + " · Hakobu"
}

// dotClass is the status dot for a container's state.
func dotClass(status string) string {
	switch status {
	case "running", "ready", "success":
		return "dot dot-success"
	case "restarting":
		return "dot dot-attention"
	case "deploying":
		return "dot dot-accent dot-pulse"
	}
	return "dot dot-danger"
}

func mb(b int64) string { return fmt.Sprintf("%.1f MB", float64(b)/(1<<20)) }

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

func itoa(n int64) string { return fmt.Sprint(n) }

// shortHost is a URL without its scheme, for showing.
func shortHost(u string) string {
	return strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
}

func sizeClass(small bool) string {
	if small {
		return "icon-sm"
	}
	return ""
}

// deployLabel says how a deploy went and what started it.
func deployLabel(d store.DeployLog) string {
	switch d.Status {
	case "success":
		return "Deployed · " + d.Trigger
	case "running":
		return "Deploying · " + d.Trigger
	}
	return "Deploy failed · " + d.Trigger
}

func keyPlaceholder(st store.Storage) string {
	if st.AccessKeyID != "" {
		return "now " + st.AccessKeyID
	}
	return ""
}

func keysLabel(st store.Storage) string {
	if st.AccessKeyID != "" {
		return "Replace keys"
	}
	return "Save keys"
}

func dbPlaceholder(suggested string) string {
	if suggested != "" {
		return suggested + " (or type another name)"
	}
	return "e.g. main"
}

func portValue(port int64) string {
	if port > 0 {
		return itoa(port)
	}
	return ""
}

func portPlaceholder(live int64) string {
	if live > 0 {
		return "auto (" + itoa(live) + ")"
	}
	return "auto"
}

func limitValue(v int64) string { return portValue(v) }

func cpuValue(v float64) string {
	if v > 0 {
		return fmt.Sprint(v)
	}
	return ""
}

func workerName(p appPage) string {
	if p.Worker != nil {
		return p.Worker.Name
	}
	return "worker"
}

func workerCommand(p appPage) string {
	if p.Worker != nil {
		return string(p.Worker.Command)
	}
	return ""
}

func workerEnv(p appPage) string {
	if p.Worker != nil {
		return string(p.Worker.Env)
	}
	return ""
}

func mountOf(p appPage, volume string) string {
	for _, v := range p.Volumes {
		if v.Name == volume {
			return v.MountPath
		}
	}
	return ""
}

func deleteAppText(p appPage) string {
	return deleteAppHint(len(p.Volumes)) + " This can't be undone."
}

// flashClass spells each kind's class out, so Tailwind keeps them.
func flashClass(kind string) string {
	switch kind {
	case "info":
		return "flash flash-info"
	case "success":
		return "flash flash-success"
	case "warn":
		return "flash flash-warn"
	case "danger":
		return "flash flash-danger"
	}
	return "flash"
}

func diskText(disk string, low bool) string {
	if low {
		return disk + ": running low, deploys and databases fail on a full disk"
	}
	return disk
}

func storageOption(st store.Storage) string {
	if st.AccessKeyID == "" {
		return st.Name + " (" + st.Provider + ", needs keys)"
	}
	return st.Name + " (" + st.Provider + ")"
}

func deleteAppHint(volumes int) string {
	if volumes > 0 {
		return "Its containers are removed and its " + plural(volumes, "volume is", "volumes are") + " deleted with their data."
	}
	return "Its containers are removed."
}

func noTracesText(rng string) string {
	if rng == "24h" {
		return "No traces received in the last 24 hours."
	}
	return "No traces received."
}

// backupLink is the line from a database's or volume's card to its row
// in the backups card, when backups are on.
func backupLink(backupsOn bool, row string) string {
	if backupsOn {
		return row
	}
	return ""
}

// logo draws a technology's logo from the sprite (logos.svg).
func logo(slug string) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		if !strings.Contains(iconSprite, `id="l-`+slug+`"`) {
			return fmt.Errorf("no logo %q in logos.svg", slug)
		}
		_, err := io.WriteString(w, `<svg class="logo" aria-hidden="true"><use href="#l-`+slug+`"/></svg>`)
		return err
	})
}

// stackLogos are the logos of what internal/detect names, frameworks first.
var stackLogos = map[string]string{
	"Next.js": "nextdotjs", "Nuxt": "nuxt", "SvelteKit": "svelte", "Svelte": "svelte", "Remix": "remix",
	"Astro": "astro", "NestJS": "nestjs", "React": "react", "Vite": "vite", "Express": "express",
	"Fastify": "fastify", "Hono": "hono", "Django": "django", "FastAPI": "fastapi", "Flask": "flask",
	"Streamlit": "streamlit", "Laravel": "laravel", "Rails": "rubyonrails",
	"Node.js": "nodedotjs", "Python": "python", "Go": "go", "Elixir": "elixir", "Static site": "html5",
}

// appLogo is the logo for an app's stack ("Python · FastAPI"): the
// framework's, else the language's, else Docker's for a Dockerfile build;
// "" if none fits.
func appLogo(a store.App) string {
	parts := strings.Split(a.Stack, " · ")
	for i := len(parts) - 1; i >= 0; i-- {
		if slug := stackLogos[strings.TrimSpace(parts[i])]; slug != "" {
			return slug
		}
	}
	if a.BuildStrategy == "dockerfile" {
		return "docker"
	}
	return ""
}

// workerLogo is Celery's for a Celery worker, else the language's: a
// worker of a FastAPI app isn't FastAPI.
func workerLogo(a store.App, command string) string {
	if strings.Contains(command, "celery") {
		return "celery"
	}
	lang, _, _ := strings.Cut(a.Stack, " · ")
	return stackLogos[lang]
}

func storageLogo(st store.Storage) string {
	if st.Provider == "r2" {
		return "cloudflare"
	}
	return ""
}

// appIcon stands in for an app's logo: public or private.
func appIcon(a appView) string {
	if a.Domain != "" {
		return "globe"
	}
	return "lock"
}

// projectCrumb is a project in the top bar, with a menu of the others.
func projectCrumb(project string) crumb {
	return crumb{Label: project, Href: "/projects/" + project, Switch: "/switch/projects?current=" + url.QueryEscape(project)}
}

// resourceCrumb is an app, database or storage in the top bar, with a menu
// of the rest of its project.
func resourceCrumb(project, name, href, logoSlug, iconName string) crumb {
	return crumb{Label: name, Href: href, Logo: logoSlug, Icon: iconName,
		Switch: "/switch/resources?project=" + url.QueryEscape(project) + "&current=" + url.QueryEscape(name)}
}
