package node

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBuildDir(t *testing.T) {
	clone := t.TempDir()
	if err := os.MkdirAll(filepath.Join(clone, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{"escape": "/etc", "alias": "web"} {
		if err := os.Symlink(target, filepath.Join(clone, link)); err != nil {
			t.Fatal(err)
		}
	}
	for path, ok := range map[string]bool{"": true, ".": true, "/web/": true, "alias": true, "escape": false, "../..": false, "missing": false} {
		if _, err := buildDir(clone, path); (err == nil) != ok {
			t.Errorf("buildDir(%q) error = %v", path, err)
		}
	}
}

func TestDockerVolumeNamesAreUnambiguous(t *testing.T) {
	if Volume("a-b", "c") == Volume("a", "b-c") {
		t.Error("different app/volume pairs map to the same Docker volume")
	}
	if got := Volume("web", "data"); got != "hakobu-vol-web_data" {
		t.Errorf("Volume = %q", got)
	}
}
