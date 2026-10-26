package node

import (
	"context"

	"github.com/hakobudev/hakobu/internal/deploy"
)

// DatabaseSizes are the bytes each database on the shared Postgres takes,
// by name.
func (Local) DatabaseSizes(ctx context.Context) (map[string]int64, error) {
	return deploy.PostgresSizes(ctx, PostgresContainer)
}

// CacheMemory is the memory a cache uses, and its limit.
func (Local) CacheMemory(ctx context.Context, c CacheSpec) (used, limit int64, err error) {
	return deploy.ValkeyMemory(ctx, c.Name, c.Password)
}
