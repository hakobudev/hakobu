// Package node is the machine a project's containers run on, as the panel
// sees it. The panel decides and keeps the records (store, Cloudflare,
// GitHub, email); a node does the work on its machine (Docker, builds,
// files, /proc) and holds no records of its own, so each call carries all
// it needs.
//
// Local is the machine hakobu runs on, the only node so far. Operations
// move here from internal/ops one at a time; the panel's code then reaches
// Docker only through a Node, which lets a node later be another server.
package node

import (
	"context"

	"github.com/x0ryz/hakobu/internal/deploy"
)

// State is what the panel shows about a container.
type State = deploy.State

type Node interface {
	// State is the container's state; Status is "not found" for one that
	// doesn't exist and "unknown" when Docker doesn't answer.
	State(ctx context.Context, container string) State
	// Statuses is every container's status by name, in one call.
	Statuses(ctx context.Context) (map[string]string, error)
	// Logs is the last lines of the container's output.
	Logs(ctx context.Context, container string, lines int) (string, error)
}

// Local is the machine hakobu runs on.
type Local struct{}

var _ Node = Local{}

func (Local) State(ctx context.Context, container string) State {
	return deploy.ContainerState(ctx, container)
}

func (Local) Statuses(ctx context.Context) (map[string]string, error) {
	return deploy.ContainerStatuses(ctx)
}

func (Local) Logs(ctx context.Context, container string, lines int) (string, error) {
	return deploy.ContainerLogs(ctx, container, lines)
}
