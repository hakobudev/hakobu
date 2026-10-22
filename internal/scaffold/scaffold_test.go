package scaffold

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hakobudev/hakobu/internal/spec"
)

func TestInit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Kasl_CRM")
	for name, text := range map[string]string{
		"Dockerfile":                  "FROM node\nEXPOSE 3000\n",
		"package.json":                `{"dependencies":{"next":"15"}}`,
		"worker/pyproject.toml":       "[project]\nname = \"w\"\n",
		"node_modules/x/package.json": "{}",
		"CLAUDE.md":                   "# CRM\n",
		".claude/settings.json":       `{"permissions":{"deny":["Bash(rm -rf:*)"]},"model":"opus"}`,
	} {
		p := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	o := Options{Dir: dir, Server: "kasl-1", Image: "ghcr.io/kasl/crm", Version: "1.0.0-alpha.1"}
	files, err := Init(o)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Kept {
			t.Errorf("kept %s on the first run", f.Path)
		}
	}

	b, _ := os.ReadFile(filepath.Join(dir, "hakobu.toml"))
	p, err := spec.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "kasl-crm" || p.Server != "kasl-1" || len(p.Apps) != 2 || p.Apps["web"].Port != 3000 || p.Apps["worker"].Path != "worker" {
		t.Errorf("hakobu.toml:\n%s", b)
	}
	if !strings.Contains(string(b), "Node.js · Next.js, built from its Dockerfile") || !strings.Contains(string(b), "Python; it needs a Dockerfile") {
		t.Errorf("no stack comment:\n%s", b)
	}

	wf, _ := os.ReadFile(filepath.Join(dir, ".github/workflows/hakobu.yml"))
	for _, want := range []string{"branches: [main]", Workflow + "@v1.0.0-alpha.1", "version: v1.0.0-alpha.1", "image: ghcr.io/kasl/crm", "packages: write"} {
		if !strings.Contains(string(wf), want) {
			t.Errorf("workflow lacks %q:\n%s", want, wf)
		}
	}

	md, _ := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if !strings.HasPrefix(string(md), "# CRM\n\n"+claudeMarker) {
		t.Errorf("CLAUDE.md:\n%s", md)
	}
	var settings struct {
		Permissions struct{ Deny []string }
		Model       string
	}
	sb, _ := os.ReadFile(filepath.Join(dir, ".claude/settings.json"))
	if err := json.Unmarshal(sb, &settings); err != nil {
		t.Fatal(err)
	}
	if settings.Model != "opus" || strings.Join(settings.Permissions.Deny, ",") != "Bash(rm -rf:*),Bash(hakobu destroy:*),Bash(hakobu restore:*)" {
		t.Errorf("settings.json: %s", sb)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".mcp.json")); !strings.Contains(string(b), `"mcp"`) {
		t.Errorf(".mcp.json: %s", b)
	}

	// Again: only the skill is written, the rest is left as it is.
	_ = os.WriteFile(filepath.Join(dir, "hakobu.toml"), []byte("edited"), 0o644)
	files, err = Init(o)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Kept == (f.Path == ".claude/skills/hakobu/SKILL.md") {
			t.Errorf("second run: %+v", f)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "hakobu.toml")); string(b) != "edited" {
		t.Error("hakobu.toml was replaced")
	}
}

func TestInitEmptyRepo(t *testing.T) {
	dir := t.TempDir()
	if _, err := Init(Options{Dir: dir, Server: "s", Image: "ghcr.io/a/b", Version: "dev"}); err != nil {
		t.Fatal(err)
	}
	wf, _ := os.ReadFile(filepath.Join(dir, ".github/workflows/hakobu.yml"))
	if !strings.Contains(string(wf), "@main") || strings.Contains(string(wf), "railpack") {
		t.Errorf("workflow:\n%s", wf)
	}
	for _, bad := range []Options{{Dir: dir, Server: "S", Image: "ghcr.io/a/b"}, {Dir: dir, Server: "s", Image: "ghcr.io/a/b:main"}, {Dir: dir, Server: "s", Image: "b"}} {
		if _, err := Init(bad); err == nil {
			t.Errorf("took %+v", bad)
		}
	}
}

func TestSkill(t *testing.T) {
	if !strings.HasPrefix(skill, "---\nname: hakobu\ndescription: ") {
		t.Error("the skill has no frontmatter")
	}
}
