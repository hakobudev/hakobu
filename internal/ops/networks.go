package ops

import (
	"fmt"

	"github.com/x0ryz/hakobu/internal/node"
	"github.com/x0ryz/hakobu/internal/store"
)

// Each project has its own networks on its node (see node.ProjectNetwork).

func ProjectNetwork(project string) string { return node.ProjectNetwork(project) }
func projectEdge(project string) string    { return node.ProjectEdge(project) }

// ensureProjectNetworks creates the project's networks on its server and
// connects the shared containers to them; the tunnels' cloudflared follow
// which account the project is in (see node.Local.EnsureProjectNetworks).
func ensureProjectNetworks(s *store.Store, projectID int64) error {
	p, err := s.GetProjectByID(ctx(), projectID)
	if err != nil {
		return err
	}
	sv, err := projectServer(s, p)
	if err != nil {
		return err
	}
	tunnels, err := tunnelSpecs(s, sv, 0)
	if err != nil {
		return err
	}
	return sv.node().EnsureProjectNetworks(ctx(), p.Key(), tunnels)
}

// ensureServerNetworks is ensureProjectNetworks for every project on the
// server, e.g. after a shared service or cloudflared was (re)created.
func ensureServerNetworks(s *store.Store, sv server) error {
	projects, err := s.ListProjects(ctx())
	if err != nil {
		return err
	}
	for _, p := range projects {
		if p.NodeID.Int64 != sv.ID {
			continue
		}
		if err := ensureProjectNetworks(s, p.ID); err != nil {
			return fmt.Errorf("networks of project %s: %w", p.Name, err)
		}
	}
	return nil
}

// removeProjectNetworks takes the shared containers off a deleted
// project's networks and removes them.
func removeProjectNetworks(s *store.Store, p store.Project) error {
	sv, err := projectServer(s, p)
	if err != nil {
		return err
	}
	tunnels, err := tunnelSpecs(s, sv, p.ID)
	if err != nil {
		return err
	}
	return sv.node().RemoveProjectNetworks(ctx(), p.Key(), tunnels)
}
