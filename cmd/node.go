package cmd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/link"
	"github.com/x0ryz/hakobu/internal/node"
	"github.com/x0ryz/hakobu/internal/ops"
	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/update"
)

// A server other than the panel's runs `hakobu node`: it keeps a link to
// the panel and does what the panel asks of it there (internal/node). It
// joins once with `hakobu node join`.

const (
	nodeKeyFile    = "key/node.key"   // the node's own key on the link
	nodeConfigFile = "data/node.json" // the panel it joined
)

type nodeConfig struct {
	Panel    string `json:"panel"`     // https://<the panel's address>
	PanelKey string `json:"panel_key"` // base64 of its ed25519 public key
}

var nodeCmd = &cobra.Command{
	Use:   "node",
	Short: "Run this server as one of a panel's servers (after `hakobu node join`)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, key, err := loadNode()
		if err != nil {
			return err
		}
		if err := prepareNode(); err != nil {
			return err
		}
		stopping, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
		defer stop()
		return runNode(stopping, cfg, key)
	},
}

var nodeJoinCmd = &cobra.Command{
	Use:   "join <panel address> <token>",
	Short: "Join a panel with the token it made for this server",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		panel := strings.TrimSuffix(strings.TrimSpace(args[0]), "/")
		if !strings.HasPrefix(panel, "https://") && !strings.HasPrefix(panel, "http://") {
			panel = "https://" + panel
		}
		tok, err := link.ParseToken(args[1])
		if err != nil {
			return err
		}
		if err := config.PrepareDataDir(); err != nil {
			return err
		}
		key, err := nodeKey()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		panelVersion, err := link.Join(ctx, panel, key, tok.PanelKey, tok.Secret, version)
		if err != nil {
			return err
		}
		if err := saveNode(nodeConfig{Panel: panel, PanelKey: base64.StdEncoding.EncodeToString(tok.PanelKey)}); err != nil {
			return err
		}
		fmt.Printf("Joined the panel at %s (hakobu %s). Run `hakobu node` to keep this server connected.\n", panel, panelVersion)
		return nil
	},
}

func init() {
	nodeCmd.AddCommand(nodeJoinCmd)
	rootCmd.AddCommand(nodeCmd)
}

// nodeKey is the node's key, made the first time.
func nodeKey() (ed25519.PrivateKey, error) {
	if b, err := os.ReadFile(nodeKeyFile); err == nil {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(raw) != ed25519.PrivateKeySize {
			return nil, fmt.Errorf("%s is corrupt", nodeKeyFile)
		}
		return ed25519.PrivateKey(raw), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key, err := link.NewKey()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(nodeKeyFile), 0o700); err != nil {
		return nil, err
	}
	return key, os.WriteFile(nodeKeyFile, []byte(base64.StdEncoding.EncodeToString(key)+"\n"), 0o600)
}

func saveNode(cfg nodeConfig) error {
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return os.WriteFile(nodeConfigFile, b, 0o600)
}

func loadNode() (nodeConfig, ed25519.PrivateKey, error) {
	var cfg nodeConfig
	b, err := os.ReadFile(nodeConfigFile)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil, errors.New("this server hasn't joined a panel: run `hakobu node join <panel address> <token>` with a token from the panel's Settings → Servers")
	}
	if err != nil {
		return cfg, nil, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, nil, fmt.Errorf("%s: %w", nodeConfigFile, err)
	}
	key, err := nodeKey()
	return cfg, key, err
}

// prepareNode readies what the node's work needs: its data directory and
// its own master key, which seals the database snapshots kept here (the
// panel never reads them; backups are sealed with keys the panel gives).
func prepareNode() error {
	if err := config.PrepareDataDir(); err != nil {
		return err
	}
	return secret.LoadKey(config.MasterKeyFile)
}

// runNode keeps the node linked to the panel, serving its calls, until
// ctx ends; a dropped link is dialled again, waiting longer each time it
// fails, up to a minute.
func runNode(ctx context.Context, cfg nodeConfig, key ed25519.PrivateKey) error {
	raw, err := base64.StdEncoding.DecodeString(cfg.PanelKey)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return fmt.Errorf("%s: the panel's key is corrupt", nodeConfigFile)
	}
	panelKey := ed25519.PublicKey(raw)
	// The panel tells its address when it moves and on each connection;
	// the node connects at the newest one.
	var mu sync.Mutex
	node.PanelMoved = func(url string) error {
		if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
			return fmt.Errorf("not a panel address: %q", url)
		}
		mu.Lock()
		defer mu.Unlock()
		if url == cfg.Panel {
			return nil
		}
		next := cfg
		next.Panel = url
		if err := saveNode(next); err != nil {
			return err
		}
		fmt.Println("the panel moved from", cfg.Panel, "to", url)
		cfg = next
		return nil
	}
	panelURL := func() string {
		mu.Lock()
		defer mu.Unlock()
		return cfg.Panel
	}
	// The apps' errors and traces come in through the ingest relay, a
	// container the panel starts here, and go on to the panel over the
	// link while there is one.
	var toPanel atomic.Pointer[http.Client]
	spool := newIngestSpool(ingestSpoolDir)
	ingestSock, err := listenSocket(ops.IngestSocketDir, filepath.Base(ops.IngestSocket()))
	if err != nil {
		return err
	}
	go func() {
		srv := &http.Server{Handler: forwardIngest(&toPanel, spool), ReadTimeout: config.ReadTimeout, WriteTimeout: config.ReadTimeout}
		_ = srv.Serve(ingestSock)
	}()
	for wait := time.Second; ctx.Err() == nil; {
		dial, cancel := context.WithTimeout(ctx, time.Minute)
		sess, panel, err := link.Dial(dial, panelURL(), key, panelKey, "", version)
		cancel()
		if err != nil {
			fmt.Printf("can't reach the panel (trying again in %s): %v\n", wait, err)
			select {
			case <-ctx.Done():
			case <-time.After(wait):
			}
			wait = min(2*wait, time.Minute)
			continue
		}
		wait = time.Second
		fmt.Println("connected to the panel at", panelURL()+", hakobu", panel.Version)
		noteLinked(panel.Version)
		if panel.URL != "" {
			if err := node.PanelMoved(panel.URL); err != nil {
				fmt.Println("the panel's new address isn't kept:", err)
			}
		}
		linked := &http.Client{Transport: &http.Transport{
			DialContext: func(context.Context, string, string) (net.Conn, error) { return sess.Open() },
		}}
		toPanel.Store(linked)
		go func() {
			n, err := spool.drain(linked)
			if n > 0 || err != nil {
				fmt.Printf("sent the panel %d envelopes kept while it was away", n)
				if err != nil {
					fmt.Printf(" (the rest wait: %v)", err)
				}
				fmt.Println()
			}
		}()
		go func() {
			select {
			case <-ctx.Done():
				sess.Close()
			case <-sess.CloseChan():
			}
		}()
		_ = node.Serve(sess, node.Local{}, sess.Open)
		toPanel.Store(nil)
		sess.Close()
		if ctx.Err() == nil {
			fmt.Println("lost the panel; connecting again")
		}
	}
	return nil
}

// forwardIngest passes the envelopes the ingest relay brings on to the
// panel over the link; while the panel is out of reach they're kept in
// spool, sent on when the node is linked again.
func forwardIngest(toPanel *atomic.Pointer[http.Client], spool *ingestSpool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxEnvelopeBytes+1))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(body) > maxEnvelopeBytes {
			http.Error(w, "envelope too large", http.StatusRequestEntityTooLarge)
			return
		}
		keep := func(why string) {
			if err := spool.put(r.URL.RequestURI(), r.Header, body); err != nil {
				fmt.Println("an envelope is lost:", why+", and keeping it failed:", err)
				http.Error(w, why, http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, "{}")
		}
		c := toPanel.Load()
		if c == nil {
			keep("not connected to the panel")
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, "http://panel"+r.URL.RequestURI(), bytes.NewReader(body))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for _, h := range ingestHeaders {
			if v := r.Header.Get(h); v != "" {
				req.Header.Set(h, v)
			}
		}
		resp, err := c.Do(req)
		if err != nil {
			keep("the panel isn't reachable")
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 500 {
			keep("the panel answered " + resp.Status)
			return
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, io.LimitReader(resp.Body, 64<<10))
	})
}

// noteLinked marks the link up (an update waits for it, see
// update.LinkedFile) and keeps the node at its panel's version: a panel
// newer than this node gets it updated, through the same request a
// panel's Update button makes, which installs the latest signed release.
// A node never goes back to an older panel's version by itself.
func noteLinked(panelVersion string) {
	now := time.Now()
	if err := os.WriteFile(update.LinkedFile, nil, 0o600); err == nil {
		_ = os.Chtimes(update.LinkedFile, now, now)
	}
	switch {
	case version == "dev" || panelVersion == "dev":
		// A build of the source: its versions say nothing.
	case update.Newer(update.Tag(panelVersion), update.Tag(version)):
		if !update.UnitsInstalled("hakobu-update.path") {
			fmt.Println("the panel runs hakobu", panelVersion, "and this server", version+": update it with sudo /opt/hakobu/hakobu update")
			return
		}
		fmt.Println("the panel runs hakobu", panelVersion+": updating this server from", version)
		if err := os.WriteFile(update.RequestFile, nil, 0o600); err != nil {
			fmt.Println("asking for the update failed:", err)
		}
	case update.Newer(update.Tag(version), update.Tag(panelVersion)):
		fmt.Println("this server runs hakobu", version, "and the panel", panelVersion+": update the panel")
	}
}
