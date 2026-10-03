package ops

import (
	"database/sql"
	"errors"
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
func ensureProjectNetworks(s *store.Store, project string) error {
	p, err := s.GetProject(ctx(), project)
	if err != nil {
		return err
	}
	sv, err := projectServer(s, p)
	if err != nil {
		return err
	}
	tunnels, err := tunnelSpecs(s, sv, "")
	if err != nil {
		return err
	}
	return sv.node().EnsureProjectNetworks(ctx(), project, tunnels)
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
		if err := ensureProjectNetworks(s, p.Name); err != nil {
			return fmt.Errorf("networks of project %s: %w", p.Name, err)
		}
	}
	return nil
}

// removeProjectNetworks takes the shared containers off a deleted
// project's networks and removes them; a project already gone from the
// records was on the panel's server.
func removeProjectNetworks(s *store.Store, project string) error {
	var sv server
	if p, err := s.GetProject(ctx(), project); err == nil {
		if sv, err = projectServer(s, p); err != nil {
			return err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	tunnels, err := tunnelSpecs(s, sv, project)
	if err != nil {
		return err
	}
	return sv.node().RemoveProjectNetworks(ctx(), project, tunnels)
}
