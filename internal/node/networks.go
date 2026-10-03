package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/deploy"
)

// Networks and the containers shared between projects. Each project's
// networks (ProjectNetwork, ProjectEdge) hold its apps; Postgres and the
// ingest relay join every project's network, and the cloudflared of the
// project's Cloudflare account its edge network, under the panel's say.
//
// A client's tunnel is theirs to edit, so its cloudflared has no panel
// socket and is on no edge network but its projects'. The panel passes
// every tunnel's whole wanted state (TunnelSpec) so the node can keep that
// true itself, also when Docker won't connect or disconnect a container
// that is restarting again and again.

// TunnelContainer runs the panel's cloudflared; a client's is
// TunnelContainer-<client>. Tests use other names.
var TunnelContainer = "hakobu-cloudflared"

// TunnelSpec is a tunnel's cloudflared as it should be.
type TunnelSpec struct {
	Client      string // "" for the panel's tunnel
	Token       string
	PanelSocket string   // the directory of the panel's socket, mounted for the panel's tunnel only
	Projects    []string // whose edge networks it's on
}

// Container runs the tunnel's cloudflared.
func (t TunnelSpec) Container() string { return TunnelName(t.Client) }

// TunnelName is the container of the client's tunnel ("" for the panel's).
func TunnelName(client string) string {
	if client == "" {
		return TunnelContainer
	}
	return TunnelContainer + "-" + client
}

// network is the one cloudflared starts on, before joining the edge
// networks of its projects.
func (t TunnelSpec) network() string {
	if t.Client == "" {
		return deploy.EdgeNetwork
	}
	return deploy.EdgeNetwork + "-" + t.Client
}

func (t TunnelSpec) serves(project string) bool {
	for _, p := range t.Projects {
		if p == project {
			return true
		}
	}
	return false
}

// RunTunnel runs the tunnel's cloudflared, or starts it again if it
// already has the tunnel's token. A new one is on exactly its projects'
// edge networks from the start.
func (n Local) RunTunnel(ctx context.Context, t TunnelSpec) error {
	if t.Token == "" {
		return nil
	}
	env, err := deploy.ContainerEnv(ctx, t.Container())
	if err != nil {
		return err
	}
	if env != nil && env["TUNNEL_TOKEN"] == t.Token {
		return deploy.StartContainer(ctx, t.Container())
	}
	return n.recreateTunnel(ctx, t)
}

func (Local) recreateTunnel(ctx context.Context, t TunnelSpec) error {
	edges := make([]string, len(t.Projects))
	for i, p := range t.Projects {
		edges[i] = ProjectEdge(p)
	}
	_, err := deploy.RunTunnelContainer(ctx, t.Container(), config.CloudflaredImage, t.Token, t.network(), t.PanelSocket, edges)
	return err
}

// RemoveTunnel stops the client's cloudflared and removes its network.
func (Local) RemoveTunnel(ctx context.Context, client string) error {
	t := TunnelSpec{Client: client}
	if err := deploy.RemoveContainer(ctx, t.Container()); err != nil {
		return err
	}
	return deploy.RemoveNetwork(ctx, t.network())
}

// EnsureProjectNetworks creates the project's networks and connects the
// shared services that run to them: Postgres and the ingest relay to its
// network, the cloudflared of the tunnel that serves the project to its
// edge network. Every other tunnel's cloudflared is taken off it (after
// the project moved to another account).
func (n Local) EnsureProjectNetworks(ctx context.Context, project string, tunnels []TunnelSpec) error {
	net, edge := ProjectNetwork(project), ProjectEdge(project)
	for _, nw := range []string{net, edge} {
		if err := deploy.EnsureNetwork(ctx, nw); err != nil {
			return err
		}
	}
	for _, c := range []string{PostgresContainer, IngestContainer} {
		if err := connectIfThere(ctx, c, net); err != nil {
			return err
		}
	}
	for _, t := range tunnels {
		if st, _ := deploy.ContainerStatus(ctx, t.Container()); st == "not found" || st == "unknown" {
			continue
		}
		var err error
		if t.serves(project) {
			err = deploy.ConnectNetwork(ctx, t.Container(), edge)
		} else {
			err = deploy.DisconnectNetwork(ctx, t.Container(), edge)
		}
		// Docker connects and disconnects only a running container; one
		// that isn't is made anew with the right networks.
		if errors.Is(err, deploy.ErrNotRunning) || errors.Is(err, deploy.ErrStaleNetwork) {
			err = n.recreateTunnel(ctx, t)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func connectIfThere(ctx context.Context, container, network string) error {
	if st, _ := deploy.ContainerStatus(ctx, container); st == "not found" || st == "unknown" {
		return nil
	}
	return deploy.ConnectNetwork(ctx, container, network)
}

// RemoveProjectNetworks takes the shared containers off a deleted
// project's networks and removes them; tunnels no longer list it.
func (n Local) RemoveProjectNetworks(ctx context.Context, project string, tunnels []TunnelSpec) error {
	for _, nw := range []string{ProjectNetwork(project), ProjectEdge(project)} {
		for _, c := range []string{PostgresContainer, IngestContainer} {
			if err := deploy.DisconnectNetwork(ctx, c, nw); err != nil {
				return err
			}
		}
		for _, t := range tunnels {
			err := deploy.DisconnectNetwork(ctx, t.Container(), nw)
			if errors.Is(err, deploy.ErrStaleNetwork) {
				err = n.recreateTunnel(ctx, t)
			}
			if err != nil {
				return err
			}
		}
		if err := deploy.RemoveNetwork(ctx, nw); err != nil {
			return err
		}
	}
	return nil
}

// The ingest relay passes apps' error and trace envelopes on to the
// panel's ingest socket; apps reach it as IngestContainer:IngestPort on
// their project's network.
// IngestNetwork is the relay's own; it joins the projects' too. Tests use
// other names.
var (
	IngestContainer = "hakobu-ingest"
	IngestNetwork   = "hakobu-ingest"
)

const IngestPort = deploy.IngestRelayPort

// StartIngestRelay runs the relay afresh, so it runs the binary of this
// hakobu, passing envelopes on to socket. The panel then connects it to
// the projects' networks (EnsureProjectNetworks).
func (Local) StartIngestRelay(ctx context.Context, socket string) error {
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	if bin, err = filepath.EvalSymlinks(bin); err != nil {
		return err
	}
	if socket, err = filepath.Abs(socket); err != nil {
		return err
	}
	if err := deploy.EnsureNetwork(ctx, IngestNetwork); err != nil {
		return err
	}
	if err := deploy.RunIngestRelay(ctx, IngestContainer, IngestNetwork, bin, socket); err != nil {
		return fmt.Errorf("ingest relay: %w", err)
	}
	return nil
}
