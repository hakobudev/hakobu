package ops

import (
	"errors"
	"fmt"

	"github.com/x0ryz/hakobu/internal/deploy"
	"github.com/x0ryz/hakobu/internal/node"
	"github.com/x0ryz/hakobu/internal/store"
)

// Each project has its own networks on its node (see node.ProjectNetwork).

func ProjectNetwork(project string) string { return node.ProjectNetwork(project) }
func projectEdge(project string) string    { return node.ProjectEdge(project) }

// ensureProjectNetworks creates the project's networks and connects the
// shared services and the ingest relay to them, whichever of those run,
// and the cloudflared of the project's account to its edge network; any
// other account's cloudflared is taken off it (after the project moved).
func ensureProjectNetworks(s *store.Store, project string) error {
	net, edge := ProjectNetwork(project), projectEdge(project)
	for _, n := range []string{net, edge} {
		if err := deploy.EnsureNetwork(ctx(), n); err != nil {
			return err
		}
	}
	for container, network := range map[string]string{PostgresContainer: net, ingestContainer: net} {
		if st, _ := deploy.ContainerStatus(ctx(), container); st == "not found" || st == "unknown" {
			continue
		}
		if err := deploy.ConnectNetwork(ctx(), container, network); err != nil {
			return err
		}
	}
	tunnels := tunnelAccounts(s)
	if len(tunnels) == 0 {
		return nil
	}
	owner, err := projectAccountByName(s, project)
	if err != nil {
		return err
	}
	for _, a := range tunnels {
		if st, _ := deploy.ContainerStatus(ctx(), a.container()); st == "not found" || st == "unknown" {
			continue
		}
		if a.ID == owner.ID {
			err = deploy.ConnectNetwork(ctx(), a.container(), edge)
		} else {
			err = deploy.DisconnectNetwork(ctx(), a.container(), edge)
		}
		// Docker connects and disconnects only a running container; one
		// that isn't (restarting again and again, say) is made anew with
		// the right networks. A tunnel must never keep reaching a project
		// that moved to another account.
		if errors.Is(err, deploy.ErrNotRunning) || errors.Is(err, deploy.ErrStaleNetwork) {
			err = runTunnel(s, a, "")
		}
		if err != nil {
			return err
		}
	}
	return nil
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
	for _, n := range []string{ProjectNetwork(project), projectEdge(project)} {
		for _, c := range []string{PostgresContainer, ingestContainer} {
			if err := deploy.DisconnectNetwork(ctx(), c, n); err != nil {
				return err
			}
		}
		for _, a := range tunnelAccounts(s) {
			err := deploy.DisconnectNetwork(ctx(), a.container(), n)
			if errors.Is(err, deploy.ErrStaleNetwork) {
				err = runTunnel(s, a, project)
			}
			if err != nil {
				return err
			}
		}
		if err := deploy.RemoveNetwork(ctx(), n); err != nil {
			return err
		}
	}
	return nil
}
