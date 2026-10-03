package detect

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestScan(t *testing.T) {
	files := []string{
		"docker-compose.yml", "README.md",
		"agent/pyproject.toml", "agent/src/main.py",
		"app/Dockerfile", "app/package.json", "app/src/deep/Dockerfile",
		"docs/guide.md",
	}
	content := map[string]string{
		"agent/pyproject.toml": `dependencies = ["fastapi>=0.110", "uvicorn"]`,
		"app/package.json":     `{"dependencies": {"next": "15.0.0"}}`,
		"app/Dockerfile":       "FROM node:22\nEXPOSE 3000\nCMD npm start",
	}
	got := Scan(files, func(p string) string { return content[p] })
	want := []Preset{
		{Path: "agent", Strategy: "railpack", Stack: "Python · FastAPI"},
		{Path: "app", Strategy: "dockerfile", Stack: "Node.js · Next.js", Port: 3000},
		{Path: "app", Strategy: "railpack", Stack: "Node.js · Next.js"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Scan =\n%+v\nwant\n%+v", got, want)
	}
}

func TestStackOf(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"package.json": `{"dependencies": {"react": "^19", "react-dom": "^19"}, "devDependencies": {"vite": "^6"}}`,
		"Dockerfile":   "FROM node:22\nEXPOSE 3000\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := StackOf(dir); got != "Node.js · React" {
		t.Errorf("StackOf = %q", got)
	}
	if got := StackOf(t.TempDir()); got != "" {
		t.Errorf("StackOf(empty) = %q", got)
	}

	// A repo's marker that is a symlink, to /dev/zero say, isn't followed.
	linked := t.TempDir()
	if err := os.Symlink("/dev/zero", filepath.Join(linked, "package.json")); err != nil {
		t.Fatal(err)
	}
	if got := StackOf(linked); got != "" {
		t.Errorf("StackOf(symlink) = %q", got)
	}
}
