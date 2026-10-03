package nodetest

import (
	"net"
	"net/http"
	"testing"

	"github.com/hashicorp/yamux"

	"github.com/x0ryz/hakobu/internal/node"
)

// Linked is n as the panel reaches a node on another server: a
// node.Remote whose calls go over yamux streams to n served as a node
// serves itself (node.Serve), with the link's encryption left out
// (internal/link has its own tests).
func Linked(t testing.TB, n node.Node) node.Node {
	t.Helper()
	r, closeLink, err := Link(n)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeLink)
	return r
}

// Link is Linked for where there's no test to clean up after (TestMain).
func Link(n node.Node) (node.Node, func(), error) {
	panelEnd, nodeEnd := net.Pipe()
	panelSess, err := yamux.Client(panelEnd, nil)
	if err != nil {
		return nil, nil, err
	}
	nodeSess, err := yamux.Server(nodeEnd, nil)
	if err != nil {
		return nil, nil, err
	}
	go func() { _ = node.Serve(nodeSess, n, nodeSess.Open) }()
	r := node.NewRemote(panelSess.Open)
	go func() { _ = http.Serve(panelSess, r.Handler()) }()
	return r, func() {
		panelSess.Close()
		nodeSess.Close()
	}, nil
}
