package main

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// The pages in site/docs are what the Markdown in docs/ makes: run
// `go run ./tools/docsgen` after changing either.
func TestPagesAreUpToDate(t *testing.T) {
	out := t.TempDir()
	if err := build("../../docs", out, "../../site"); err != nil {
		t.Fatal(err)
	}
	want := map[string][]byte{}
	_ = filepath.WalkDir(out, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(out, p)
			want[rel], _ = os.ReadFile(p)
		}
		return nil
	})
	have := map[string]bool{}
	_ = filepath.WalkDir("../../site/docs", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel("../../site/docs", p)
			have[rel] = true
			b, _ := os.ReadFile(p)
			if w, ok := want[rel]; !ok {
				t.Errorf("site/docs/%s is made from nothing in docs/", rel)
			} else if !bytes.Equal(b, w) {
				t.Errorf("site/docs/%s is behind docs/: run go run ./tools/docsgen", rel)
			}
		}
		return nil
	})
	for rel := range want {
		if !have[rel] {
			t.Errorf("site/docs/%s is missing: run go run ./tools/docsgen", rel)
		}
	}
}

// Links between pages point at pages that exist.
func TestLinksBetweenPages(t *testing.T) {
	items, err := readNav("../../docs/nav.md")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, it := range items {
		names[it.Name] = true
	}
	for _, it := range items {
		src, _ := os.ReadFile(filepath.Join("../../docs", it.Name+".md"))
		for _, m := range linkInMD.FindAllSubmatch(src, -1) {
			if !names[string(m[1])] {
				t.Errorf("%s.md links to %s.md, which isn't in nav.md", it.Name, m[1])
			}
		}
	}
}
