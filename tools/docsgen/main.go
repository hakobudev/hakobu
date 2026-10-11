// Command docsgen builds hakobu.dev/docs from the Markdown in docs/: one
// page per file, at /docs/<name>/ (index.md at /docs/), with the menu in
// docs/nav.md and the site's look (site/style.css, site/docs.css).
//
//	go run ./tools/docsgen
//
// The pages are committed under site/docs/; a test fails when they're
// behind the Markdown.
package main

import (
	"bufio"
	"bytes"
	"fmt"
	"html"
	"html/template"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	gmhtml "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

func main() {
	if err := build("docs", "site/docs", "site"); err != nil {
		log.Fatal(err)
	}
	// AI assistants look for llms.txt at the root.
	b, err := os.ReadFile("site/docs/llms.txt")
	if err == nil {
		err = os.WriteFile("site/llms.txt", b, 0o644)
	}
	if err != nil {
		log.Fatal(err)
	}
}

// navItem is a page in the menu, under its section.
type navItem struct {
	Section, Title, Name string
}

// readNav reads nav.md: "## Section" lines, then "- [Title](name.md)".
func readNav(path string) ([]navItem, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	link := regexp.MustCompile(`^- \[(.+)\]\(([a-z0-9-]+)\.md\)$`)
	var items []navItem
	section := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "## "):
			section = strings.TrimPrefix(line, "## ")
		case link.MatchString(line):
			m := link.FindStringSubmatch(line)
			items = append(items, navItem{section, m[1], m[2]})
		case line != "" && !strings.HasPrefix(line, "#"):
			return nil, fmt.Errorf("%s: can't read %q", path, line)
		}
	}
	return items, sc.Err()
}

// url is where a page is served.
// siteURL is where the docs are published.
const siteURL = "https://hakobu.dev"

func url(name string) string {
	if name == "index" {
		return "/docs/"
	}
	return "/docs/" + name + "/"
}

// heading is an h2 or h3 of a page, for its "On this page".
type heading struct {
	Level    int
	ID, Text string
}

// mdLinks points links to other pages (name.md, name.md#part) at theirs.
type mdLinks struct{}

var (
	mdLink   = regexp.MustCompile(`^([a-z0-9-]+)\.md(#.*)?$`)
	linkInMD = regexp.MustCompile(`\]\(([a-z0-9-]+)\.md(?:#[^)]*)?\)`)
)

func (mdLinks) Transform(doc *ast.Document, reader text.Reader, pc parser.Context) {
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if l, ok := n.(*ast.Link); ok && entering {
			if m := mdLink.FindSubmatch(l.Destination); m != nil {
				l.Destination = []byte(url(string(m[1])) + string(m[2]))
			}
		}
		return ast.WalkContinue, nil
	})
}

var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(parser.WithAutoHeadingID(), parser.WithASTTransformers(util.Prioritized(mdLinks{}, 100))),
	goldmark.WithRendererOptions(gmhtml.WithUnsafe()),
)

// render turns a page's Markdown into its title, body and headings.
func render(src []byte) (title string, body template.HTML, toc []heading, err error) {
	doc := md.Parser().Parse(text.NewReader(src))
	var h1 ast.Node // the title, shown above the page: taken out after the walk
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		h, ok := n.(*ast.Heading)
		if !ok || !entering {
			return ast.WalkContinue, nil
		}
		t := string(h.Lines().Value(src))
		t = strings.NewReplacer("`", "", "**", "").Replace(t)
		if h.Level == 1 && title == "" {
			title, h1 = t, h
			return ast.WalkSkipChildren, nil
		}
		if id, ok := h.AttributeString("id"); ok && (h.Level == 2 || h.Level == 3) {
			toc = append(toc, heading{h.Level, string(id.([]byte)), t})
		}
		return ast.WalkSkipChildren, nil
	})
	if title == "" {
		return "", "", nil, fmt.Errorf("no # title")
	}
	h1.Parent().RemoveChild(h1.Parent(), h1)
	var b bytes.Buffer
	if err := md.Renderer().Render(&b, src, doc); err != nil {
		return "", "", nil, err
	}
	return title, template.HTML(b.String()), toc, nil
}

// page is what the template gets.
type page struct {
	Title, Description, URL, Version string
	Body                             template.HTML
	TOC                              []heading
	Nav                              []navSection
	Prev, Next                       *navItem
}

type navSection struct {
	Title string
	Items []navLink
}

type navLink struct {
	Title, URL string
	Current    bool
}

// build writes a page for every item of src/nav.md into out.
func build(src, out, site string) error {
	items, err := readNav(filepath.Join(src, "nav.md"))
	if err != nil {
		return err
	}
	tmpl, err := template.New("page").Funcs(template.FuncMap{"url": url}).Parse(pageTemplate)
	if err != nil {
		return err
	}
	version, err := assetsVersion(site)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(out); err != nil {
		return err
	}
	var index, full bytes.Buffer
	index.WriteString("# hakobu\n\n> hakobu deploys apps from GitHub to servers you own, behind Cloudflare: builds, zero-downtime swaps, databases, backups only your servers can open, errors and traces, and an MCP server for Claude.\n")
	section := ""
	for i, it := range items {
		raw, err := os.ReadFile(filepath.Join(src, it.Name+".md"))
		if err != nil {
			return err
		}
		title, body, toc, err := render(raw)
		if err != nil {
			return fmt.Errorf("%s.md: %w", it.Name, err)
		}
		p := page{Title: title, Description: describe(raw), URL: url(it.Name), Version: version, Body: body, TOC: toc}
		for _, o := range items {
			if len(p.Nav) == 0 || p.Nav[len(p.Nav)-1].Title != o.Section {
				p.Nav = append(p.Nav, navSection{Title: o.Section})
			}
			s := &p.Nav[len(p.Nav)-1]
			s.Items = append(s.Items, navLink{o.Title, url(o.Name), o.Name == it.Name})
		}
		if i > 0 {
			p.Prev = &items[i-1]
		}
		if i < len(items)-1 {
			p.Next = &items[i+1]
		}
		dir := filepath.Join(out, strings.TrimPrefix(url(it.Name), "/docs/"))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		// The page as Markdown, for AI assistants: links point at the
		// other pages' Markdown on hakobu.dev.
		md := linkInMD.ReplaceAllFunc(raw, func(m []byte) []byte {
			sub := linkInMD.FindSubmatch(m)
			return []byte("](" + siteURL + url(string(sub[1])) + "index.md)")
		})
		if err := os.WriteFile(filepath.Join(dir, "index.md"), md, 0o644); err != nil {
			return err
		}
		if it.Section != section {
			section = it.Section
			fmt.Fprintf(&index, "\n## %s\n\n", section)
		}
		fmt.Fprintf(&index, "- [%s](%s%sindex.md): %s\n", title, siteURL, url(it.Name), p.Description)
		full.Write(bytes.TrimSpace(md))
		full.WriteString("\n\n")
		var b bytes.Buffer
		if err := tmpl.Execute(&b, p); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "index.html"), b.Bytes(), 0o644); err != nil {
			return err
		}
	}
	// llms.txt: what the docs hold, for AI assistants; llms-full.txt: all
	// of it in one file.
	if err := os.WriteFile(filepath.Join(out, "llms.txt"), index.Bytes(), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(out, "llms-full.txt"), bytes.TrimSpace(full.Bytes()), 0o644)
}

// describe is a page's first paragraph, as plain text, for its meta
// description.
func describe(src []byte) string {
	for _, para := range strings.Split(string(src), "\n\n") {
		para = strings.TrimSpace(para)
		if para == "" || strings.HasPrefix(para, "#") || strings.HasPrefix(para, "```") || strings.HasPrefix(para, "-") {
			continue
		}
		para = regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`).ReplaceAllString(para, "$1")
		para = strings.NewReplacer("`", "", "**", "", "\n", " ").Replace(para)
		if len(para) > 200 {
			para = para[:strings.LastIndex(para[:200], " ")] + "…"
		}
		return html.UnescapeString(para)
	}
	return ""
}

// assetsVersion is a short hash of the site's stylesheets, so a browser
// fetches them anew when they change.
func assetsVersion(site string) (string, error) {
	var all []byte
	for _, f := range []string{"style.css", "docs.css"} {
		b, err := os.ReadFile(filepath.Join(site, f))
		if err != nil {
			return "", err
		}
		all = append(all, b...)
	}
	return fmt.Sprintf("%08x", fnv32(all)), nil
}

func fnv32(b []byte) uint32 {
	h := uint32(2166136261)
	for _, c := range b {
		h = (h ^ uint32(c)) * 16777619
	}
	return h
}

const pageTemplate = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} · hakobu docs</title>
<meta name="description" content="{{.Description}}">
<meta name="color-scheme" content="light dark">
<link rel="preload" href="/fonts/inter.woff2" as="font" type="font/woff2" crossorigin>
<link rel="stylesheet" href="/style.css?v={{.Version}}">
<link rel="stylesheet" href="/docs.css?v={{.Version}}">
</head>
<body class="docs">
<header class="topbar">
  <div class="wrap docs-wrap topbar-inner">
    <span class="docs-brand"><a href="/" class="wordmark">hakobu</a><a href="/docs/" class="docs-label">Docs</a></span>
    <nav class="nav">
      <a class="nav-link nav-hide" href="/#self-host">Self-host</a>
      <a class="nav-link" href="https://github.com/hakobudev/hakobu" aria-label="GitHub"><svg class="icon" viewBox="0 0 24 24"><path d="M15 22v-4a4.8 4.8 0 0 0-1-3.5c3 0 6-2 6-5.5.08-1.25-.27-2.48-1-3.5.28-1.15.28-2.35 0-3.5 0 0-1 0-3 1.5-2.64-.5-5.36-.5-8 0C6 2 5 2 5 2c-.3 1.15-.3 2.35 0 3.5A5.403 5.403 0 0 0 4 9c0 3.5 3 5.5 6 5.5-.39.49-.68 1.05-.85 1.65-.17.6-.22 1.23-.15 1.85v4"/><path d="M9 18c-4.51 2-5-2-7-2"/></svg></a>
      <a class="btn" href="https://dash.hakobu.dev">Sign in</a>
    </nav>
  </div>
</header>
<div class="wrap docs-wrap docs-layout">
  <details class="docs-menu" open>
    <summary>Menu</summary>
    <nav class="docs-nav" aria-label="Documentation">
      {{- range .Nav}}
      <div class="docs-nav-section">{{.Title}}</div>
      {{- range .Items}}
      <a href="{{.URL}}"{{if .Current}} aria-current="page"{{end}}>{{.Title}}</a>
      {{- end}}
      {{- end}}
    </nav>
  </details>
  <main class="docs-main">
    <article class="prose">
      <div class="docs-head">
        <h1>{{.Title}}</h1>
        <div class="docs-md">
          <button type="button" class="btn btn-sm" id="copy-md">Copy as Markdown</button>
          <a class="btn btn-sm" href="index.md">View as Markdown</a>
        </div>
      </div>
      {{.Body}}
    </article>
    <nav class="docs-pager" aria-label="Pages">
      {{- with .Prev}}<a class="docs-prev" href="{{url .Name}}"><span>Previous</span>{{.Title}}</a>{{else}}<span></span>{{end}}
      {{- with .Next}}<a class="docs-next" href="{{url .Name}}"><span>Next</span>{{.Title}}</a>{{end}}
    </nav>
  </main>
  {{- if .TOC}}
  <aside class="docs-toc">
    <div class="docs-toc-title">On this page</div>
    {{- range .TOC}}
    <a href="#{{.ID}}" class="toc-{{.Level}}">{{.Text}}</a>
    {{- end}}
  </aside>
  {{- end}}
</div>
<footer class="footer">
  <div class="wrap docs-wrap footer-inner">
    <span><span class="wordmark">hakobu</span>&nbsp; Open source, MIT</span>
    <nav>
      <a href="https://github.com/hakobudev/hakobu">GitHub</a>
      <a href="/docs/">Docs</a>
      <a href="/privacy">Privacy</a>
      <a href="/terms">Terms</a>
    </nav>
  </div>
</footer>
<script>
// The menu is open on wide screens, a toggle on narrow ones.
const menu = document.querySelector(".docs-menu");
if (matchMedia("(max-width: 860px)").matches) menu.open = false;

// The page as Markdown, to paste into an AI assistant.
const copy = document.querySelector("#copy-md");
copy.addEventListener("click", async () => {
  try {
    const md = await (await fetch("index.md")).text();
    await navigator.clipboard.writeText(md);
    copy.textContent = "Copied";
  } catch {
    copy.textContent = "Couldn't copy";
  }
  setTimeout(() => { copy.textContent = "Copy as Markdown"; }, 1500);
});
</script>
</body>
</html>
`
