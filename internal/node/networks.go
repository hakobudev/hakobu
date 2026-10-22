package node

import (
	"context"

	"github.com/hakobudev/hakobu/internal/deploy"
)

// EnsureProjectNetworks creates the project's network and connects the
// shared Postgres to it, if it runs.
func (Local) EnsureProjectNetworks(ctx context.Context, project string) error {
	net := ProjectNetwork(project)
	if err := deploy.EnsureNetwork(ctx, net); err != nil {
		return err
	}
	if st, _ := deploy.ContainerStatus(ctx, PostgresContainer); st == "not found" || st == "unknown" {
		return nil
	}
	return deploy.ConnectNetwork(ctx, PostgresContainer, net)
}
