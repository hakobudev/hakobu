// Package detect finds buildable directories in a repo (its root and
// top-level subdirectories) and names their stack.
package detect

import (
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type Preset struct {
	Path     string // "." or a top-level directory
	Strategy string // "dockerfile" or "railpack"
	Stack    string // e.g. "Python · FastAPI"
	Port     int    // from the Dockerfile's EXPOSE, 0 if unknown
}

// languages maps a marker file to the stack Railpack builds from it; the
// first match in this order wins.
var languages = []struct{ file, name string }{
	{"package.json", "Node.js"},
	{"deno.json", "Deno"},
	{"pyproject.toml", "Python"},
	{"requirements.txt", "Python"},
	{"Pipfile", "Python"},
	{"go.mod", "Go"},
	{"Cargo.toml", "Rust"},
	{"Gemfile", "Ruby"},
	{"composer.json", "PHP"},
	{"mix.exs", "Elixir"},
	{"pom.xml", "Java"},
	{"build.gradle", "Java"},
	{"build.gradle.kts", "Java"},
	{"index.html", "Static site"},
}

// frameworks are recognized by a dependency name appearing in the marker file.
var frameworks = map[string][]struct{ dep, name string }{
	"Node.js": {{`"next"`, "Next.js"}, {`"nuxt"`, "Nuxt"}, {`"@sveltejs/kit"`, "SvelteKit"}, {`"@remix-run/`, "Remix"}, {`"astro"`, "Astro"}, {`"@nestjs/core"`, "NestJS"}, {`"vite"`, "Vite"}, {`"express"`, "Express"}, {`"fastify"`, "Fastify"}, {`"hono"`, "Hono"}},
	"Python":  {{"django", "Django"}, {"fastapi", "FastAPI"}, {"flask", "Flask"}, {"litestar", "Litestar"}, {"streamlit", "Streamlit"}},
	"PHP":     {{"laravel/framework", "Laravel"}, {"symfony/", "Symfony"}},
	"Ruby":    {{"rails", "Rails"}},
}

var exposeRe = regexp.MustCompile(`(?im)^\s*EXPOSE\s+(\d+)`)

// Scan detects presets from the repo's file list; read returns a file's
// content ("" if unavailable) and is only called for a few marker files.
func Scan(files []string, read func(path string) string) []Preset {
	dirs := map[string]map[string]bool{}
	for _, f := range files {
		dir, name := path.Split(f)
		dir = strings.TrimSuffix(dir, "/")
		if dir == "" {
			dir = "."
		}
		if dir != "." && strings.Contains(dir, "/") {
			continue
		}
		if dirs[dir] == nil {
			dirs[dir] = map[string]bool{}
		}
		dirs[dir][name] = true
	}

	var order []string
	for d := range dirs {
		order = append(order, d)
	}
	sort.Slice(order, func(i, j int) bool { return order[i] == "." || (order[j] != "." && order[i] < order[j]) })

	var presets []Preset
	for _, dir := range order {
		names := dirs[dir]
		join := func(name string) string {
			if dir == "." {
				return name
			}
			return dir + "/" + name
		}
		stack := ""
		for _, l := range languages {
			if names[l.file] {
				stack = l.name
				if fws, ok := frameworks[l.name]; ok {
					content := strings.ToLower(read(join(l.file)))
					for _, fw := range fws {
						if strings.Contains(content, strings.ToLower(fw.dep)) {
							stack += " · " + fw.name
							break
						}
					}
				}
				break
			}
		}
		if names["Dockerfile"] {
			p := Preset{Path: dir, Strategy: "dockerfile", Stack: stack}
			if m := exposeRe.FindStringSubmatch(read(join("Dockerfile"))); m != nil {
				p.Port, _ = strconv.Atoi(m[1])
			}
			presets = append(presets, p)
		}
		if stack != "" {
			presets = append(presets, Preset{Path: dir, Strategy: "railpack", Stack: stack})
		}
	}
	return presets
}
