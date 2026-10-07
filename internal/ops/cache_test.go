package ops

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// A cache runs on its project's node with a password of its own; a linked
// app gets REDIS_URL; a new memory cap or password runs it again; it can't
// go while an app uses it, and goes with its project.
func TestCaches(t *testing.T) {
	s, f := fakeNode(t)
	p, _ := getProject(s, "shop")
	id, err := CreateCache(s, p.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	c, _ := s.GetCacheByID(ctx(), id)
	spec, ok := f.Caches[c.Key()]
	if c.Name != "shop-cache" || !ok || spec.Project != p.Key() || spec.Password != string(c.Password) || len(spec.Password) < 32 || spec.MemoryMB != DefaultCacheMemoryMB {
		t.Fatalf("cache %+v runs as %+v", c, spec)
	}
	if _, err := CreateCache(s, p.ID, "shop-cache"); err == nil {
		t.Error("a second cache took the same name")
	}

	web := webApp(t, s)
	if err := LinkCache(s, web, "nope"); err == nil {
		t.Error("linked a cache that doesn't exist")
	}
	if err := LinkCache(s, web, c.Name); err != nil {
		t.Fatal(err)
	}
	env, err := appEnv(s, webApp(t, s), 3000)
	if err != nil {
		t.Fatal(err)
	}
	url := "REDIS_URL=redis://default:" + string(c.Password) + "@" + c.Key() + ":6379/0"
	if !slices.Contains(env, url) || !slices.Contains(env, "REDIS_HOST="+c.Key()) {
		t.Errorf("the app's environment lacks the cache: %v", env)
	}

	if err := SetCacheMemory(s, id, 8); err == nil {
		t.Error("took a memory cap of 8 MB")
	}
	if err := SetCacheMemory(s, id, 512); err != nil || f.Caches[c.Key()].MemoryMB != 512 {
		t.Errorf("memory: %v, runs with %d", err, f.Caches[c.Key()].MemoryMB)
	}
	if err := rotateCachePassword(s, c); err != nil {
		t.Fatal(err)
	}
	if c2, _ := s.GetCacheByID(ctx(), id); c2.Password == c.Password || f.Caches[c.Key()].Password != string(c2.Password) {
		t.Error("a new password wasn't given to the cache")
	}

	f.Fail("EnsureCache", errors.New("boom"))
	if err := SetCacheMemory(s, id, 1024); err == nil {
		t.Error("a cap the node couldn't apply was taken")
	}
	if c2, _ := s.GetCacheByID(ctx(), id); c2.MemoryMB != 512 {
		t.Errorf("the panel keeps %d MB after the node failed", c2.MemoryMB)
	}
	f.Fail("EnsureCache", nil)

	if err := DeleteCache(s, id); err == nil || !strings.Contains(err.Error(), "web") {
		t.Errorf("deleted a cache an app uses: %v", err)
	}
	if err := LinkCache(s, webApp(t, s), ""); err != nil {
		t.Fatal(err)
	}
	if err := DeleteCache(s, id); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Caches[c.Key()]; ok {
		t.Error("the deleted cache still runs")
	}

	id, err = CreateCache(s, p.ID, "jobs")
	if err != nil {
		t.Fatal(err)
	}
	c, _ = s.GetCacheByID(ctx(), id)
	if err := DeleteProject(s, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Caches[c.Key()]; ok {
		t.Error("a deleted project's cache still runs")
	}
	if _, err := s.GetCacheByID(ctx(), id); err == nil {
		t.Error("a deleted project's cache is still recorded")
	}
}
