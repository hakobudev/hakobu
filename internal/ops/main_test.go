package ops

import (
	"fmt"
	"os"
	"testing"

	"github.com/x0ryz/hakobu/internal/deploy"
	"github.com/x0ryz/hakobu/internal/node"
	"github.com/x0ryz/hakobu/internal/node/nodetest"
)

// TestMain gives the shared containers and networks names of their own, so
// tests never touch a real hakobu's Postgres, cloudflared or
// networks on the same Docker.
func TestMain(m *testing.M) {
	deploy.RunDialerIfChild() // rootless Docker: this binary is the dialer too
	id := fmt.Sprint(os.Getpid())
	node.PostgresContainer = "zt-postgres-" + id
	node.TunnelContainer = "zt-cloudflared-" + id
	node.IngestContainer, node.IngestNetwork = "zt-ingest-"+id, "zt-ingest-"+id
	deploy.NetworkName = "zt-hakobu-" + id
	deploy.EdgeNetwork = "zt-hakobu-edge-" + id
	// HAKOBU_TEST_LINK=1: the panel reaches this machine as it would a node
	// on another server, over the link's protocol.
	closeLink := func() {}
	if os.Getenv("HAKOBU_TEST_LINK") != "" {
		r, c, err := nodetest.Link(node.Local{})
		if err != nil {
			panic(err)
		}
		local, closeLink = r, c
	}
	code := m.Run()
	closeLink()
	for _, n := range []string{deploy.NetworkName, deploy.EdgeNetwork, node.IngestNetwork} {
		_ = deploy.RemoveNetwork(ctx(), n) // only there if a test used it
	}
	os.Exit(code)
}
