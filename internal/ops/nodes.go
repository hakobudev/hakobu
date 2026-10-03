package ops

import (
	"database/sql"
	"errors"
	"fmt"
	"net"

	"github.com/x0ryz/hakobu/internal/node"
	"github.com/x0ryz/hakobu/internal/store"
)

// local is the machine hakobu runs on: the panel's server.
var local node.Node = node.Local{}

// server is where a project runs: the panel's own server (Name "") or one
// that joined it.
type server struct {
	ID   int64 // 0 for the panel's
	Name string
}

func (sv server) label() string {
	if sv.Name == "" {
		return "the panel's server"
	}
	return "server " + sv.Name
}

// node is the server's node: local, the connected server's, or one whose
// every call fails with node.ErrUnreachable while it isn't connected.
func (sv server) node() node.Node {
	if sv.Name == "" {
		return local
	}
	if n, err := serverNode(sv.Name); err == nil {
		return n
	}
	return offline(sv.Name)
}

// offline is a server that isn't connected.
func offline(name string) node.Node {
	return node.NewRemote(func() (net.Conn, error) {
		return nil, fmt.Errorf("server %s isn't connected", name)
	})
}

func projectServer(s *store.Store, p store.Project) (server, error) {
	if !p.NodeID.Valid {
		return server{}, nil
	}
	n, err := s.GetNode(ctx(), p.NodeID.Int64)
	if err != nil {
		return server{}, fmt.Errorf("the server of project %s: %w", p.Name, err)
	}
	return server{ID: n.ID, Name: n.Name}, nil
}

// ProjectNode is the node the project's apps, databases and volumes are on.
func ProjectNode(s *store.Store, project string) node.Node {
	p, err := s.GetProject(ctx(), project)
	if err != nil {
		return local
	}
	sv, err := projectServer(s, p)
	if err != nil {
		return offline(fmt.Sprint(p.NodeID.Int64))
	}
	return sv.node()
}

// AppNode is ProjectNode for the app's project.
func AppNode(s *store.Store, app store.App) node.Node {
	return ProjectNode(s, app.ProjectName)
}

// onLocal reports whether the app runs on the panel's server.
func onLocal(s *store.Store, app store.App) bool {
	p, err := s.GetProject(ctx(), app.ProjectName)
	return err == nil && !p.NodeID.Valid
}

// placedNode is a node the panel watches, with what its usage is recorded
// under.
type placedNode struct {
	server
	n node.Node
}

// allNodes are the nodes the panel watches now: the panel's server, then
// the servers connected.
func allNodes(s *store.Store) []placedNode {
	out := []placedNode{{server{}, local}}
	rows, err := s.ListNodes(ctx())
	if err != nil {
		return out
	}
	for _, r := range rows {
		if n, err := serverNode(r.Name); err == nil {
			out = append(out, placedNode{server{ID: r.ID, Name: r.Name}, n})
		}
	}
	return out
}

// SetProjectServer moves a project to a server ("" for the panel's), only
// while it has no apps or databases: their data would stay behind.
func SetProjectServer(s *store.Store, project, serverName string) error {
	p, err := s.GetProject(ctx(), project)
	if err != nil {
		return fmt.Errorf("project %q not found: %w", project, err)
	}
	var id sql.NullInt64
	if serverName != "" {
		n, err := s.GetNodeByName(ctx(), serverName)
		if err != nil {
			return fmt.Errorf("server %q not found", serverName)
		}
		if n.PublicKey == "" {
			return fmt.Errorf("server %s hasn't joined yet", serverName)
		}
		id = sql.NullInt64{Int64: n.ID, Valid: true}
	}
	if id == p.NodeID {
		return nil
	}
	apps, err := s.ListAppsByProject(ctx(), p.ID)
	if err != nil {
		return err
	}
	dbs, err := s.ListDatabasesByProject(ctx(), p.ID)
	if err != nil {
		return err
	}
	if len(apps)+len(dbs) > 0 {
		return errors.New("only a project without apps and databases can move to another server: their data would stay behind")
	}
	return s.SetProjectNode(ctx(), store.SetProjectNodeParams{NodeID: id, ID: p.ID})
}
