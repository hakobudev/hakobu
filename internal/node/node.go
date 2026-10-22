// Package node is the machine hakobu runs on: Docker, its containers,
// networks and volumes, PostgreSQL and the caches. It holds no records of
// its own, so each call carries all it needs.
package node

import (
	"context"

	"github.com/hakobudev/hakobu/internal/deploy"
)

// State is a container's state.
type State = deploy.State

// Local is the machine hakobu runs on.
type Local struct{}

func (Local) Logs(ctx context.Context, container string, lines int) (string, error) {
	return deploy.ContainerLogs(ctx, container, lines)
}
