package ops

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/x0ryz/hakobu/internal/node"
	"github.com/x0ryz/hakobu/internal/panellog"
	"github.com/x0ryz/hakobu/internal/store"
)

// Apps reach hakobu's ingest endpoint inside the server, through the ingest
// relay (cmd/ingest_relay.go): a container on every project's network that
// passes envelopes on to IngestSocket.

// IngestSocketDir holds the ingest socket; only the relay mounts it.
const IngestSocketDir = "run/ingest"

// IngestSocket serves the ingest endpoint and nothing else.
func IngestSocket() string { return filepath.Join(IngestSocketDir, "ingest.sock") }

// ingestDSN is the SENTRY_DSN of an app on this server.
func ingestDSN(key string, appID int64) string {
	return fmt.Sprintf("http://%s@%s:%d/%d", key, node.IngestContainer, node.IngestPort, appID)
}

// KeepIngestRelay starts the relay with this hakobu's binary, trying again
// until Docker answers.
func KeepIngestRelay(s *store.Store) {
	for wait := 5 * time.Second; ; wait = min(2*wait, 5*time.Minute) {
		err := startIngestRelay(s)
		if err == nil {
			return
		}
		panellog.Warnf("ingest relay not started (trying again in %s): %v", wait, err)
		time.Sleep(wait)
	}
}

// startIngestRelay runs the relay afresh on the panel's node, so it runs
// the binary of this hakobu, and connects it to the projects' networks.
func startIngestRelay(s *store.Store) error {
	if err := local.StartIngestRelay(ctx(), IngestSocket()); err != nil {
		return err
	}
	return ensureServerNetworks(s, server{})
}
