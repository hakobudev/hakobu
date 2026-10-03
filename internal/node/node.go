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
	"io"

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

	// Builds (apps.go).
	Build(ctx context.Context, b BuildSpec, out io.Writer) (stack string, err error)
	ExposedPort(ctx context.Context, app string, img Image) int64
	HasImage(ctx context.Context, app string, img Image) (bool, error)
	DropImage(ctx context.Context, app string, img Image)
	Promote(ctx context.Context, app string) error
	SwapForRollback(ctx context.Context, app string) error

	// Rolling out a version: StartCandidate, then the panel records the
	// candidate's slot as live, then Switch and Retire; Discard if the
	// panel can't record it or Switch fails.
	StartCandidate(ctx context.Context, app AppSpec, img Image, out io.Writer) (Candidate, error)
	Switch(ctx context.Context, app AppSpec, c Candidate) error
	Retire(ctx context.Context, app AppSpec, c Candidate, out io.Writer)
	Discard(ctx context.Context, app AppSpec, c Candidate, out io.Writer, err error) error

	// The live version.
	StopApp(ctx context.Context, app AppSpec) error
	StartApp(ctx context.Context, app AppSpec, out io.Writer)
	EnsureProxy(ctx context.Context, app AppSpec)
	RemoveProxy(app string)
	Reconcile(ctx context.Context, apps []AppSpec)

	// Workers.
	RunWorker(ctx context.Context, w WorkerSpec, out io.Writer) error
	RemoveWorker(ctx context.Context, app string) error
	// RemoveApp deletes everything of the app's on the node, with the
	// given volumes' data.
	RemoveApp(ctx context.Context, app string, volumes []string) error

	// Databases (data.go).
	EnsurePostgres(ctx context.Context) error
	PostgresReady(ctx context.Context) bool
	CreateDatabase(ctx context.Context, d DBSpec) error
	EnsureDatabase(ctx context.Context, d DBSpec) error
	DropDatabase(ctx context.Context, d DBSpec) error
	SetPassword(ctx context.Context, d DBSpec) error
	DumpDatabase(ctx context.Context, d DBSpec, w io.Writer) error
	RestoreDatabase(ctx context.Context, d DBSpec, r io.Reader) error
	VerifyDump(ctx context.Context, d DBSpec, r io.Reader) (tables int, err error)

	// Snapshots for Rollback with data, kept on the node.
	SaveDump(ctx context.Context, d DBSpec) (Dump, error)
	DropDump(dump Dump)
	KeepSnapshot(app string, dump Dump) error
	HasSnapshot(app string) bool
	SetAside(app string, dump Dump) string
	PruneSnapshots(apps map[string]bool)
	ReplaceDatabase(ctx context.Context, d DBSpec, dump Dump) error

	// Volumes.
	HasVolume(ctx context.Context, app, name string) (bool, error)
	ArchiveVolume(ctx context.Context, app, name string, w io.Writer) error
	RestoreVolume(ctx context.Context, app, name string, r io.Reader) error
	RemoveVolume(ctx context.Context, app, name string) error
	RemoveVolumesExcept(ctx context.Context, keep map[string]bool) error

	// The machine (host.go).
	Readings(ctx context.Context, containers map[string]bool) (Readings, error)
	Disk(ctx context.Context) (used, total uint64, err error)
	Cleanup(ctx context.Context, c CleanupSpec) (freed int64, images int, err error)
	WatchDeaths(ctx context.Context, fn func(container, app string, d Death)) error
	CheckHealth(ctx context.Context, app AppSpec, port int64) (ok bool, why string, checked bool)

	// Networks and shared containers (networks.go). tunnels is every
	// tunnel the node runs, as it should be.
	EnsureProjectNetworks(ctx context.Context, project string, tunnels []TunnelSpec) error
	RemoveProjectNetworks(ctx context.Context, project string, tunnels []TunnelSpec) error
	RunTunnel(ctx context.Context, t TunnelSpec) error
	RemoveTunnel(ctx context.Context, client string) error
	StartIngestRelay(ctx context.Context, socket string) error
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
