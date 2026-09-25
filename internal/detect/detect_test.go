package detect

import (
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
