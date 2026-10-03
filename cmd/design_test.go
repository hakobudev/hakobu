package cmd

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Pages are built from the design system's components (ui_kit.templ and
// the classes in styles.css) and layout utilities. A color, font size,
// weight, radius, background, border or shadow written into a page is how
// every screen ends up looking a little different, so it fails here: give
// the component a variant in styles.css instead.
func TestPagesUseTheDesignSystem(t *testing.T) {
	forbidden := regexp.MustCompile(`^(?:[a-z]+:)*(?:` +
		`text-(?:xs|sm|base|lg|[2-9]?xl|\[.*\])` + // sizes: .small, .page-title, .section-title, ...
		`|text-(?:fg|muted|accent|canvas|subtle|line|current|transparent)` + // colors other than the status ones
		`|font-(?:thin|light|normal|medium|semibold|bold|black)` + // weights: .strong, .row-title, ...
		`|rounded(?:-.*)?|bg-.*|border(?:-.*)?|shadow(?:-.*)?|ring(?:-.*)?|opacity-.*` +
		`)$`)
	// Literal strings inside class attributes: class="a b" and the strings
	// of class={ "a", templ.KV("b", x) }.
	attr := regexp.MustCompile(`class=(?:"([^"]*)"|\{([^}]*)\})`)
	literal := regexp.MustCompile(`"([^"]*)"`)
	files, _ := filepath.Glob("*.templ")
	if len(files) == 0 {
		t.Fatal("no templ files")
	}
	for _, f := range files {
		if f == "ui_kit.templ" {
			continue // the design system itself
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			for _, m := range attr.FindAllStringSubmatch(line, -1) {
				classes := m[1]
				if m[2] != "" {
					for _, l := range literal.FindAllStringSubmatch(m[2], -1) {
						classes += " " + l[1]
					}
				}
				for _, c := range strings.Fields(classes) {
					if forbidden.MatchString(c) {
						t.Errorf("%s:%d: class %q bypasses the design system; use a component or add a variant in styles.css", f, i+1, c)
					}
				}
			}
		}
	}
}
