package ops

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/x0ryz/hakobu/internal/store"
)

// Tests name things the way people do; their names are unique across the
// test's store, so these find them without the project or user.

func createProject(s *store.Store, name string) error {
	_, err := s.CreateProject(ctx(), store.CreateProjectParams{Name: name})
	return err
}

func getProject(s *store.Store, name string) (store.Project, error) {
	projects, err := s.ListProjects(ctx())
	if err != nil {
		return store.Project{}, err
	}
	for _, p := range projects {
		if p.Name == name {
			return p, nil
		}
	}
	return store.Project{}, sql.ErrNoRows
}

func createApp(s *store.Store, p store.CreateAppParams) error {
	_, err := s.CreateApp(ctx(), p)
	return err
}

func getApp(s *store.Store, name string) (store.App, error) {
	apps, err := s.ListApps(ctx())
	if err != nil {
		return store.App{}, err
	}
	for _, a := range apps {
		if a.Name == name {
			return a, nil
		}
	}
	return store.App{}, sql.ErrNoRows
}

func getDB(s *store.Store, name string) (store.Database, error) {
	dbs, err := s.ListDatabases(ctx())
	if err != nil {
		return store.Database{}, err
	}
	for _, d := range dbs {
		if d.Name == name {
			return d, nil
		}
	}
	return store.Database{}, sql.ErrNoRows
}

func getStorage(s *store.Store, name string) (store.Storage, error) {
	all, err := s.ListStorages(ctx())
	if err != nil {
		return store.Storage{}, err
	}
	for _, st := range all {
		if st.Name == name {
			return st, nil
		}
	}
	return store.Storage{}, sql.ErrNoRows
}

// mustID is the ID of what get finds by name, failing the test otherwise.
func mustID[T any](t *testing.T, get func(*store.Store, string) (T, error), s *store.Store, name string, id func(T) int64) int64 {
	t.Helper()
	v, err := get(s, name)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return id(v)
}

func appID(t *testing.T, s *store.Store, name string) int64 {
	t.Helper()
	return mustID(t, getApp, s, name, func(a store.App) int64 { return a.ID })
}

func projectID(t *testing.T, s *store.Store, name string) int64 {
	t.Helper()
	return mustID(t, getProject, s, name, func(p store.Project) int64 { return p.ID })
}

func dbID(t *testing.T, s *store.Store, name string) int64 {
	t.Helper()
	return mustID(t, getDB, s, name, func(d store.Database) int64 { return d.ID })
}

func mustApp(t *testing.T, s *store.Store, name string) store.App {
	t.Helper()
	a, err := getApp(s, name)
	if err != nil {
		t.Fatalf("app %s: %v", name, err)
	}
	return a
}

var _ = fmt.Sprint

// seedApp makes project (if new) and the app name in it, returning it.
func seedApp(t *testing.T, s *store.Store, project, name string) store.App {
	t.Helper()
	p, err := getProject(s, project)
	if err != nil {
		if err := createProject(s, project); err != nil {
			t.Fatal(err)
		}
		p, _ = getProject(s, project)
	}
	if err := createApp(s, store.CreateAppParams{ProjectID: p.ID, Name: name, BuildStrategy: "dockerfile"}); err != nil {
		t.Fatal(err)
	}
	return mustApp(t, s, name)
}

// serverNamed is the joined server of that name, the zero server if none.
func serverNamed(s *store.Store, name string) server {
	rows, _ := s.ListNodes(ctx())
	for _, n := range rows {
		if n.Name == name {
			return server{ID: n.ID, Name: n.Name}
		}
	}
	return server{}
}

// must2 drops the ID a create returns, keeping its error for must.
func must2(_ int64, err error) error { return err }
