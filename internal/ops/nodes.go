package ops

import (
	"github.com/x0ryz/hakobu/internal/node"
	"github.com/x0ryz/hakobu/internal/store"
)

// local is the machine hakobu runs on; every project's node so far.
var local node.Node = node.Local{}

// ProjectNode is the node the project's apps, databases and volumes are on.
func ProjectNode(s *store.Store, project string) node.Node {
	return local
}

// AppNode is ProjectNode for the app's project.
func AppNode(s *store.Store, app store.App) node.Node {
	return ProjectNode(s, app.ProjectName)
}
