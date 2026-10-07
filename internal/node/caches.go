package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/deploy"
)

// CacheSpec is a cache as its node runs it: a Valkey server of its own,
// container Name on its project's network, where apps reach it by Name.
type CacheSpec struct {
	Name     string
	Project  string // the project's key, whose network it's on
	Password string // hakobu's own hex
	MemoryMB int64
}

// stamp sums up what the container must run with, so a changed password
// or memory cap recreates it; the password itself stays out of it.
func (c CacheSpec) stamp() string {
	sum := sha256.Sum256([]byte(c.Password + "\x00" + strconv.FormatInt(c.MemoryMB, 10) + "\x00" + config.ValkeyImage))
	return hex.EncodeToString(sum[:8])
}

// EnsureCache runs the cache as specified: started again if it already
// does, recreated over its volume (the data stays) if it runs otherwise.
func (Local) EnsureCache(ctx context.Context, c CacheSpec) error {
	env, err := deploy.ContainerEnv(ctx, c.Name)
	if err != nil {
		return err
	}
	if env != nil && env["HAKOBU_CACHE"] == c.stamp() {
		err = deploy.StartContainer(ctx, c.Name)
	} else {
		_, err = deploy.RunCacheContainer(ctx, c.Name, config.ValkeyImage, c.Password, c.MemoryMB, ProjectNetwork(c.Project), c.stamp())
	}
	if err != nil {
		return err
	}
	if !deploy.WaitHealthy(30, time.Second, func() bool { return deploy.ValkeyReady(ctx, c.Name, c.Password) }) {
		return fmt.Errorf("cache %s failed to become ready", c.Name)
	}
	return nil
}

// DropCache removes the cache's container and its data.
func (Local) DropCache(ctx context.Context, name string) error {
	if err := deploy.RemoveContainer(ctx, name); err != nil {
		return err
	}
	return deploy.RemoveVolume(ctx, name+"_data")
}
