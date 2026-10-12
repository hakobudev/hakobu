package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/hakobudev/hakobu/internal/ops"
	"github.com/hakobudev/hakobu/internal/panellog"
	"github.com/hakobudev/hakobu/internal/store"
)

// Commands run on the panel's server that need the servers it's connected
// to hand their work to the running panel: only it holds the links. They
// reach it on cliSocket, in the data directory, which only the hakobu user
// may open.
const cliSocket = "data/cli.sock"

// listenCLISocket listens on cliSocket for the hakobu user alone.
func listenCLISocket() (net.Listener, error) {
	os.Remove(cliSocket) // left by the previous run
	ln, err := net.Listen("unix", cliSocket)
	if err != nil {
		return nil, err
	}
	return ln, os.Chmod(cliSocket, 0o600)
}

// serveCLI serves the commands' requests on ln. A dump can take minutes to
// upload and seal, so there are no timeouts: only local commands connect.
func serveCLI(s *store.Store, ln net.Listener) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /databases/{id}/import", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "bad database ID", http.StatusBadRequest)
			return
		}
		if _, err := ops.ImportDump(s, id, r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	if err := (&http.Server{Handler: mux}).Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
		panellog.Error("command socket:", err)
	}
}

// cliClient reaches the running panel on cliSocket.
var cliClient = &http.Client{Transport: &http.Transport{
	DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", cliSocket)
	},
}}

// panelImport has the running panel import the dump into database id.
func panelImport(id int64, dump io.Reader) error {
	resp, err := cliClient.Post(fmt.Sprintf("http://hakobu/databases/%d/import", id), "application/octet-stream", dump)
	if err != nil {
		return fmt.Errorf("can't reach the running panel (%w): is hakobu running, and is this run as the hakobu user from /opt/hakobu?", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return errors.New(strings.TrimSpace(string(msg)))
}
