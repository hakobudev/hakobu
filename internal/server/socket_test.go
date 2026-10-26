package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hakobudev/hakobu/internal/client"
)

// On the socket, who connects is the token: the test's user is anyone
// but root, so the operator.
func TestSocket(t *testing.T) {
	d, _ := newBareDaemon(t)
	dir := t.TempDir()
	d.Config.Socket = filepath.Join(dir, "missing", "api.sock")
	if l, err := d.listenSocket(); l != nil || err != nil {
		t.Fatalf("no directory, no socket: %v %v", l, err)
	}
	d.Config.Socket = filepath.Join(dir, "api.sock")
	if err := os.WriteFile(d.Config.Socket, nil, 0o600); err != nil { // the last run's
		t.Fatal(err)
	}
	l, err := d.listenSocket()
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(d.Config.Socket); fi.Mode().Perm() != 0o660 {
		t.Errorf("socket is %v", fi.Mode())
	}
	srv := &http.Server{Handler: d.API(), ConnContext: peerContext}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })

	name, sv, ok := client.Local(d.Config.Socket)
	if !ok || name != "nova-2" {
		t.Fatalf("local: %q %v", name, ok)
	}
	c, _ := client.Servers{name: sv}.Client(name)
	if !c.Local() {
		t.Error("not local")
	}
	var st Status
	if err := c.Get(context.Background(), "/v1/status", &st); err != nil || st.Server != "nova-2" {
		t.Errorf("status: %v %+v", err, st)
	}
	err = c.Post(context.Background(), "/v1/destroy", map[string]string{"project": "nova", "kind": "database", "name": "main"}, nil)
	if os.Geteuid() != 0 && (err == nil || !strings.Contains(err.Error(), "admin")) {
		t.Errorf("destroy as anyone but root: %v", err)
	}
	// The admin token still counts, on the socket too.
	err = c.WithToken("admin-token").Post(context.Background(), "/v1/destroy", map[string]string{"project": "nova", "kind": "database", "name": "main"}, nil)
	if err != nil && strings.Contains(err.Error(), "needs the admin") {
		t.Errorf("destroy with the admin token: %v", err)
	}
}
