package node

import (
	"context"
	"os"
	"strings"

	"github.com/hakobudev/hakobu/internal/config"
	"github.com/hakobudev/hakobu/internal/secret"
)

// A node's own keys seal its backups and snapshots and never leave it
// through the panel: the owner exports them on the server (`hakobu key
// export`). The panel knows only their fingerprints, and whether the owner
// has a copy of the current one.

// KeyState is a node's keys as the panel sees them.
type KeyState struct {
	Fingerprints []string // the master key's first, then the keyring's
	Exported     bool     // the owner exported the current master key
}

// Current is the fingerprint of the key that seals now.
func (k KeyState) Current() string {
	if len(k.Fingerprints) == 0 {
		return ""
	}
	return k.Fingerprints[0]
}

func (Local) KeyState(ctx context.Context) (KeyState, error) {
	k := KeyState{Fingerprints: secret.Fingerprints()}
	b, err := os.ReadFile(config.KeyExportedFile)
	k.Exported = err == nil && k.Current() != "" && strings.TrimSpace(string(b)) == k.Current()
	return k, nil
}
