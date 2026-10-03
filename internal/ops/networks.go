package ops

import (
	"fmt"

	"github.com/x0ryz/hakobu/internal/node"
	"github.com/x0ryz/hakobu/internal/store"
)

// Each project has its own networks on its node (see node.ProjectNetwork).

func ProjectNetwork(project string) string { return node.ProjectNetwork(project) }
func projectEdge(project string) string    { return node.ProjectEdge(project) }

// ensureProjectNetworks creates the project's networks on its node and
// connects the shared containers to them; the tunnels' cloudflared follow
// which account the project is in (see node.Local.EnsureProjectNetworks).
func ensureProjectNetworks(s *store.Store, project string) error {
	tunnels, err := tunnelSpecs(s, "")
	if err != nil {
		return err
	}
	return ProjectNode(s, project).EnsureProjectNetworks(ctx(), project, tunnels)
}

// ensureAllProjectNetworks is ensureProjectNetworks for every project,
// e.g. after a shared service or cloudflared was (re)created.
func ensureAllProjectNetworks(s *store.Store) error {
	projects, err := s.ListProjects(ctx())
	if err != nil {
		return err
	}
	for _, p := range projects {
		if err := ensureProjectNetworks(s, p.Name); err != nil {
			return fmt.Errorf("networks of project %s: %w", p.Name, err)
		}
	}
	return nil
}

// removeProjectNetworks takes the shared containers off a deleted
// project's networks and removes them.
func removeProjectNetworks(s *store.Store, project string) error {
	tunnels, err := tunnelSpecs(s, project)
	if err != nil {
		return err
	}
	return ProjectNode(s, project).RemoveProjectNetworks(ctx(), project, tunnels)
}
