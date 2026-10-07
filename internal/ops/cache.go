package ops

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/hakobudev/hakobu/internal/node"
	"github.com/hakobudev/hakobu/internal/secret"
	"github.com/hakobudev/hakobu/internal/store"
)

// Caches are Valkey servers (Redis-compatible), one container each on its
// project's network and server, for caches, queues and sessions. Their
// data outlives restarts and redeploys but isn't backed up.

// DefaultCacheMemoryMB caps a new cache; its page changes it.
const DefaultCacheMemoryMB = 256

func cacheSpec(s *store.Store, c store.Cache) (node.CacheSpec, error) {
	p, err := s.GetProjectByID(ctx(), c.ProjectID)
	if err != nil {
		return node.CacheSpec{}, err
	}
	return node.CacheSpec{Name: c.Key(), Project: p.Key(), Password: string(c.Password), MemoryMB: c.MemoryMB}, nil
}

// ensureCache runs c on its project's server as the panel has it.
func ensureCache(s *store.Store, c store.Cache) error {
	spec, err := cacheSpec(s, c)
	if err != nil {
		return err
	}
	if err := ensureProjectNetworks(s, c.ProjectID); err != nil {
		return err
	}
	return ProjectNode(s, c.ProjectID).EnsureCache(ctx(), spec)
}

// SuggestCacheName is the name the "new cache" field offers, and takes
// when left empty.
func SuggestCacheName(s *store.Store, p store.Project) string {
	return suggestName(p.Name, "cache", validName, func(name string) bool {
		_, err := s.GetCache(ctx(), store.GetCacheParams{ProjectID: p.ID, Name: name})
		return err == nil
	})
}

// CreateCache creates a cache named name ("" for the suggested one) in
// the project and starts it.
func CreateCache(s *store.Store, projectID int64, name string) (int64, error) {
	p, err := s.GetProjectByID(ctx(), projectID)
	if err != nil {
		return 0, fmt.Errorf("project not found: %w", err)
	}
	if name == "" {
		name = SuggestCacheName(s, p)
	}
	if err := checkName("cache", name); err != nil {
		return 0, err
	}
	password, err := RandomHex(24)
	if err != nil {
		return 0, err
	}
	// The record first: its ID names the container.
	id, err := s.CreateCache(ctx(), store.CreateCacheParams{Name: name, ProjectID: p.ID, Password: secret.String(password), MemoryMB: DefaultCacheMemoryMB})
	if err != nil {
		return 0, taken(err, "a cache named %q", name)
	}
	c, err := s.GetCacheByID(ctx(), id)
	if err == nil {
		err = ensureCache(s, c)
	}
	if err != nil {
		_ = ProjectNode(s, p.ID).DropCache(ctx(), store.Cache{ID: id}.Key())
		_ = s.DeleteCache(ctx(), id)
		return 0, err
	}
	return id, nil
}

// SetCacheMemory gives the cache a new memory cap; it restarts with its
// data. Its apps keep their connection details.
func SetCacheMemory(s *store.Store, id, memoryMB int64) error {
	if memoryMB < 32 || memoryMB > 65536 {
		return fmt.Errorf("memory must be between 32 and 65536 MB")
	}
	c, err := s.GetCacheByID(ctx(), id)
	if err != nil {
		return fmt.Errorf("cache not found: %w", err)
	}
	old := c.MemoryMB
	c.MemoryMB = memoryMB
	if err := s.SetCacheMemory(ctx(), store.SetCacheMemoryParams{ID: id, MemoryMB: memoryMB}); err != nil {
		return err
	}
	if err := ensureCache(s, c); err != nil {
		_ = s.SetCacheMemory(ctx(), store.SetCacheMemoryParams{ID: id, MemoryMB: old})
		return err
	}
	return nil
}

// DeleteCache removes the cache with its data, once no app uses it.
func DeleteCache(s *store.Store, id int64) error {
	c, err := s.GetCacheByID(ctx(), id)
	if err != nil {
		return fmt.Errorf("cache not found: %w", err)
	}
	linked, err := s.AppsUsingCache(ctx(), sql.NullInt64{Int64: id, Valid: true})
	if err != nil {
		return err
	}
	if len(linked) > 0 {
		return fmt.Errorf("cache %s is still used by %s — unlink it first", c.Name, strings.Join(linked, ", "))
	}
	if err := ProjectNode(s, c.ProjectID).DropCache(ctx(), c.Key()); err != nil {
		return err
	}
	return s.DeleteCache(ctx(), id)
}

// LinkCache links (or with name "" unlinks) one of the project's caches,
// by its name there; the app gets it on its next deploy or restart.
func LinkCache(s *store.Store, app store.App, name string) error {
	var id sql.NullInt64
	if name != "" {
		c, err := s.GetCache(ctx(), store.GetCacheParams{ProjectID: app.ProjectID, Name: name})
		if err != nil {
			return fmt.Errorf("cache %q not found in project %s", name, app.ProjectName)
		}
		id = sql.NullInt64{Int64: c.ID, Valid: true}
	}
	return s.SetAppLinkedCache(ctx(), store.SetAppLinkedCacheParams{ID: app.ID, LinkedCacheID: id})
}

// CacheEnv is what an app linked to c gets: the usual Redis variables,
// which every Redis client reads, pointing at its Valkey.
func CacheEnv(c store.Cache) []string {
	url := fmt.Sprintf("redis://default:%s@%s:6379/0", c.Password, c.Key())
	return []string{
		"REDIS_URL=" + url,
		"VALKEY_URL=" + url,
		"REDIS_HOST=" + c.Key(),
		"REDIS_PORT=6379",
		"REDIS_PASSWORD=" + string(c.Password),
	}
}

// rotateCachePassword gives the cache a new password; it restarts with
// its data, and its apps get the password on their restart.
func rotateCachePassword(s *store.Store, c store.Cache) error {
	old := c.Password
	password, err := RandomHex(24)
	if err != nil {
		return err
	}
	c.Password = secret.String(password)
	if err := s.SetCachePassword(ctx(), store.SetCachePasswordParams{ID: c.ID, Password: c.Password}); err != nil {
		return err
	}
	if err := ensureCache(s, c); err != nil {
		_ = s.SetCachePassword(ctx(), store.SetCachePasswordParams{ID: c.ID, Password: old})
		return err
	}
	return nil
}
