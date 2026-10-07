package ops

import (
	"fmt"
	"sync"
	"time"

	"github.com/hakobudev/hakobu/internal/node"
	"github.com/hakobudev/hakobu/internal/panellog"
	"github.com/hakobudev/hakobu/internal/store"
)

// A server's backups are sealed with its own key, which the panel never
// sees: the owner exports it on the server. The panel learns from each
// server whether they did, and asks them until they have.

var serverKeys struct {
	sync.Mutex
	of map[int64]node.KeyState // by server ID
}

// noteServerKeys asks the server for its keys' state and remembers it.
func noteServerKeys(id int64, n node.Node) {
	k, err := n.KeyState(ctx())
	if err != nil {
		panellog.Warn("keys of server", id, err)
		return
	}
	serverKeys.Lock()
	defer serverKeys.Unlock()
	if serverKeys.of == nil {
		serverKeys.of = map[int64]node.KeyState{}
	}
	serverKeys.of[id] = k
}

func knownServerKeys(id int64) (node.KeyState, bool) {
	serverKeys.Lock()
	defer serverKeys.Unlock()
	k, ok := serverKeys.of[id]
	return k, ok
}

// KeyExportCommand is what the owner runs on a server to keep its keys.
func KeyExportCommand(name string) string {
	return "cd /opt/hakobu && sudo ./hakobu key export > ~/hakobu-" + name + ".key"
}

// checkServerKeys asks the connected servers again and mails the owner of
// each whose current key they have no copy of.
func checkServerKeys(s *store.Store) {
	rows, err := s.ListNodes(ctx())
	if err != nil {
		return
	}
	for _, row := range rows {
		if n, err := serverNode(server{ID: row.ID, Name: row.Name}); err == nil {
			noteServerKeys(row.ID, n)
		}
		key := fmt.Sprintf("serverkey:%d", row.ID)
		if k, ok := knownServerKeys(row.ID); ok && !k.Exported {
			problem(s, key, 7*24*time.Hour, "Save the key of server "+row.Name,
				"Server "+row.Name+" seals its backups with its own key, which hakobu never sees: without a copy of it, they can't be restored if the server is lost. On the server, run:\n\n  "+
					KeyExportCommand(row.Name)+"\n\nand keep the file away from it.\n\n"+panelURL("/settings#servers"))
		} else if ok {
			solved(s, key, "", "")
		}
	}
}
