package ops

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The panel decides and keeps the records; what runs on a machine (Docker,
// builds, backups, proxies) goes through the project's node, which may be
// another server. Tests may still reach Docker directly to check on it.
func TestOpsReachesMachinesThroughNodes(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			for _, m := range []string{"deploy", "build", "backup", "proxy", "edge"} {
				if path == "github.com/x0ryz/hakobu/internal/"+m {
					t.Errorf("%s imports %s: go through the project's node (internal/node) instead", f, path)
				}
			}
		}
	}
}
