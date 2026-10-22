// Package apply brings a server in line with its projects' hakobu.toml: plan
// says what would change, apply changes it. It never destroys data: a
// database, cache or volume gone from the file is kept, orphaned, until
// someone destroys it by name.
package apply

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/hakobudev/hakobu/internal/spec"
)

// State is what the server remembers between deploys that neither the
// file nor Docker says: the spec last applied, which slot each app serves
// from, the ports of its proxies and the passwords hakobu generated. It's
// one JSON file, written whole.
type State struct {
	Projects map[string]*ProjectState `json:"projects"`
}

type ProjectState struct {
	// Spec is the hakobu.toml last applied.
	Spec      *spec.Project             `json:"spec"`
	Apps      map[string]*AppState      `json:"apps"`
	Databases map[string]*DatabaseState `json:"databases"`
	Caches    map[string]*CacheState    `json:"caches"`
	// Volumes are the apps' volumes made so far, "app/volume", with the
	// ones the file no longer has among them.
	Volumes map[string]bool `json:"volumes"`
}

type AppState struct {
	Slot      string `json:"slot"` // "blue" or "green", the live one
	ProxyPort int64  `json:"proxy_port"`
	// Port is the one the live container answers on, for its proxy when
	// hakobu starts again.
	Port int64 `json:"port"`
	Image     string `json:"image"`    // digest live now
	Previous  string `json:"previous"` // digest before it, the rollback target
}

type DatabaseState struct {
	Password string `json:"password"`
}

type CacheState struct {
	Password string `json:"password"`
}

// LoadState reads the state file; a missing one is an empty state.
func LoadState(file string) (*State, error) {
	b, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return &State{Projects: map[string]*ProjectState{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	if s.Projects == nil {
		s.Projects = map[string]*ProjectState{}
	}
	return &s, nil
}

// Save writes the state through a temporary file and a rename, so a crash
// leaves the old state or the new one, never half of it.
func (s *State) Save(file string) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), file)
}

// Project is the project's state, made empty on first use.
func (s *State) Project(name string) *ProjectState {
	p, ok := s.Projects[name]
	if !ok {
		p = &ProjectState{}
		s.Projects[name] = p
	}
	if p.Apps == nil {
		p.Apps = map[string]*AppState{}
	}
	if p.Databases == nil {
		p.Databases = map[string]*DatabaseState{}
	}
	if p.Caches == nil {
		p.Caches = map[string]*CacheState{}
	}
	if p.Volumes == nil {
		p.Volumes = map[string]bool{}
	}
	return p
}
