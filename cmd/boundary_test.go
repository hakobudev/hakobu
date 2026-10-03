package cmd

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The panel's pages and the MCP server reach a project's machine only
// through its node (ops.ProjectNode), never Docker, builds, backups or
// proxies directly: the machine may later be another server. The agent
// is the local node's process, so its wiring may.
func TestPanelReachesMachinesThroughNodes(t *testing.T) {
	machine := []string{"deploy", "build", "backup", "proxy", "edge"}
	allowed := map[string]bool{"agent.go": true, "dialer.go": true}
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || allowed[f] {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			for _, m := range machine {
				if path == "github.com/x0ryz/hakobu/internal/"+m {
					t.Errorf("%s imports %s: go through the project's node instead", f, path)
				}
			}
		}
	}
}
