package detect

import (
	"os"
	"path/filepath"
)

type Preset struct {
	Strategy string
	Path     string
	Label    string
}

func Scan(repoDir string) ([]Preset, error) {
	var presets []Preset

	entries, err := os.ReadDir(repoDir)
	if err != nil {
		return nil, err
	}

	presets = append(presets, scanDir(repoDir, ".")...)

	for _, e := range entries {
		if !e.IsDir() || e.Name() == ".git" {
			continue
		}
		subPath := filepath.Join(repoDir, e.Name())
		presets = append(presets, scanDir(subPath, e.Name())...)
	}

	return presets, nil
}

func scanDir(fullPath, relPath string) []Preset {
	var found []Preset

	if _, err := os.Stat(filepath.Join(fullPath, "Dockerfile")); err == nil {
		found = append(found, Preset{Strategy: "dockerfile", Path: relPath, Label: "Dockerfile"})
	}

	markers := []string{"pyproject.toml", "requirements.txt", "package.json", "go.mod", "Cargo.toml"}
	for _, m := range markers {
		if _, err := os.Stat(filepath.Join(fullPath, m)); err == nil {
			found = append(found, Preset{Strategy: "railpack", Path: relPath, Label: "Railpack (auto-detect)"})
			break
		}
	}

	return found
}
