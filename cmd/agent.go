package cmd

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/edge"
	"github.com/x0ryz/hakobu/internal/link"
	"github.com/x0ryz/hakobu/internal/ops"
	"github.com/x0ryz/hakobu/internal/store"
)

var (
	agentAddr       string
	agentPublicHost string
)

var agentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Run the hakobu agent (panel, deploys, webhooks)",
	RunE:  runAgent,
}

func init() {
	agentCmd.Flags().StringVar(&agentAddr, "addr", config.AgentAddr, "listen address (or HAKOBU_ADDR)")
	agentCmd.Flags().StringVar(&agentPublicHost, "public-host", "", "public host of the panel, e.g. panel.example.com (saved to data/public_host)")
}

func runAgent(cmd *cobra.Command, args []string) error {
	if err := config.PrepareDataDir(); err != nil {
		return err
	}
	s, err := store.OpenWithKey(config.DatabaseFile, config.MasterKeyFile)
	if err != nil {
		return err
	}
	if agentPublicHost != "" {
		if err := config.SetPublicHost(agentPublicHost); err != nil {
			return err
		}
	}
	if err := s.FailRunningDeployLogs(context.Background()); err != nil {
		return err
	}

	setupURL, err := ensureSetupToken(s)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook/github", webhookHandler(s))
	registerWebRoutes(mux, s)
	ingest := ingestHandler(s)
	// Servers that joined connect here; their key, checked inside the
	// link, admits them, not a session.
	links, err := ops.LinkServer(s, version, ingest)
	if err != nil {
		return err
	}
	mux.Handle("GET "+link.Path, links)
	mux.HandleFunc("POST /api/{app_id}/envelope/", ingest)
	ingestMux := http.NewServeMux()
	ingestMux.HandleFunc("POST /api/{app_id}/envelope/", ingest)

	ln, err := net.Listen("tcp", agentAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w (is another hakobu agent running?)", agentAddr, err)
	}

	sock, err := listenPanelSocket()
	if err != nil {
		return err
	}
	ingestSock, err := listenSocket(ops.IngestSocketDir, filepath.Base(ops.IngestSocket()))
	if err != nil {
		return err
	}

	// Tidying up after the last run waits on Docker, which may be slow to
	// answer at boot: the panel serves meanwhile, and jobs wait for it.
	ops.CloseJobs("hakobu is starting")
	go func() {
		ops.SyncDatabasePasswords(s)
		ops.ReconcileSlots(s)
		ops.OpenJobs()
	}()
	go runProxyPoller(s)
	go ops.WatchDeaths(s)
	go ops.WatchHealth(s)
	go ops.WatchMetrics(s)
	go runBackupScheduler(s)
	if err := ops.StartTunnel(s); err != nil {
		fmt.Println("failed to start the tunnel:", err)
	}

	fmt.Println("hakobu listening on", ln.Addr())
	switch {
	case config.PublicHost() == "":
		fmt.Println("No panel address yet: run `hakobu setup`.")
	case setupURL != "":
		fmt.Println("Finish setup:", setupURL)
	default:
		fmt.Println("Panel: https://" + config.PublicHost())
	}
	srv := &http.Server{
		Handler:      edge.Router(s, panelHandler(mux)),
		ReadTimeout:  config.ReadTimeout,
		WriteTimeout: config.WriteTimeout,
		IdleTimeout:  config.IdleTimeout,
	}
	go func() {
		if err := srv.Serve(sock); err != nil && err != http.ErrServerClosed {
			fmt.Println("panel socket:", err)
		}
	}()
	go func() {
		ingestSrv := &http.Server{Handler: ingestMux, ReadTimeout: config.ReadTimeout, WriteTimeout: config.ReadTimeout, IdleTimeout: config.IdleTimeout}
		if err := ingestSrv.Serve(ingestSock); err != nil {
			fmt.Println("ingest socket:", err)
		}
	}()
	go ops.KeepIngestRelay(s)

	stopping, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	select {
	case err := <-served:
		return err
	case <-stopping.Done():
	}
	// systemd waits 90 seconds before killing: let requests and running
	// deploys finish meanwhile. One cut short is tidied up at the next
	// start (ReconcileSlots, FailRunningDeployLogs).
	fmt.Println("stopping: finishing requests and running deploys")
	ops.CloseJobs("hakobu is stopping")
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		fmt.Println("some requests were cut short:", err)
	}
	if !ops.WaitForJobs(time.Minute) {
		fmt.Println("a deploy was still running and is cut short; the next start tidies up after it")
	}
	return nil
}

// listenPanelSocket is where cloudflared, in its own container, reaches the
// panel: a unix socket in a directory mounted into it. The socket is open to
// every local user, like the loopback port.
func listenPanelSocket() (net.Listener, error) {
	return listenSocket(ops.PanelSocketDir, "panel.sock")
}

// listenSocket listens on a unix socket open to every local user (the
// containers mounting its directory run as other users) in dir.
func listenSocket(dir, name string) (net.Listener, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, name)
	os.Remove(path) // left by the previous run
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	return ln, os.Chmod(path, 0o666)
}

// ensureSetupToken keeps a one-time setup token on disk until an owner has
// signed in; only the holder of the setup link can connect GitHub and claim
// the panel.
func ensureSetupToken(s *store.Store) (string, error) {
	if owner, err := s.Owner(context.Background()); err != nil || owner.GitHubID != 0 {
		return "", err
	}
	token := config.SetupToken()
	if token == "" {
		var err error
		if token, err = ops.RandomHex(16); err != nil {
			return "", err
		}
		if err := config.SetSetupToken(token); err != nil {
			return "", err
		}
	}
	return "https://" + config.PublicHost() + "/setup?token=" + token, nil
}

// runProxyPoller keeps every app's proxy listening (after an agent restart
// this re-attaches them to their containers) and retries tunnel route
// updates that failed, waiting longer after each failure: one that won't
// pass, a deleted token say, would otherwise call Cloudflare every few
// seconds until it rate-limits the account.
func runProxyPoller(s *store.Store) {
	lastErr := ""
	var wait time.Duration
	var nextSync time.Time
	for {
		if apps, err := s.ListApps(context.Background()); err == nil {
			for _, a := range apps {
				if !ops.IsDeploying(a.Name) { // a running job (or deletion) owns the proxy
					ops.EnsureProxy(s, a)
				}
			}
		}
		if time.Now().After(nextSync) {
			err := ops.SyncTunnel(s)
			wait = syncBackoff(wait, err)
			nextSync = time.Now().Add(wait)
			if err != nil && err.Error() != lastErr {
				fmt.Printf("tunnel routes not updated (retrying in %s): %v\n", wait, err)
			}
			lastErr = fmt.Sprint(err)
		}
		time.Sleep(config.ProxyPollEvery)
	}
}

// syncBackoff is how long to wait before the next tunnel sync: no time
// after one that worked, twice as long as the last wait after a failure,
// from a minute up to an hour.
func syncBackoff(last time.Duration, err error) time.Duration {
	if err == nil {
		return 0
	}
	return min(max(2*last, time.Minute), time.Hour)
}

// runBackupScheduler checks hourly for databases, volumes and the panel due a
// backup (so restarts don't postpone them) and for what to email the owner
// about, and cleans up once a day.
func runBackupScheduler(s *store.Store) {
	time.Sleep(time.Minute) // let Docker and the databases come up first
	for !ops.JobsOpen() {
		time.Sleep(5 * time.Second)
	}
	lastCleanup := time.Now()
	for ; ; time.Sleep(time.Hour) {
		for name, err := range ops.BackupDue(s) {
			if err != nil {
				fmt.Println("backup failed for", name+":", err)
			}
			ops.NoteBackup(s, name, err)
		}
		for name, err := range ops.VolumeBackupDue(s) {
			if err != nil {
				fmt.Println("backup failed for volume", name+":", err)
			}
			ops.NoteBackup(s, name, err)
		}
		err := ops.PanelBackupDue(s)
		if err != nil {
			fmt.Println("panel backup failed:", err)
		}
		ops.NoteBackup(s, "panel", err)
		ops.CheckForOwner(s)
		if time.Since(lastCleanup) < 24*time.Hour {
			continue
		}
		lastCleanup = time.Now()
		if err := s.PruneOldData(context.Background(), config.RetentionDays); err != nil {
			fmt.Println("prune failed:", err)
		}
		if err := ops.Cleanup(s); err != nil {
			fmt.Println("cleanup failed:", err)
		}
	}
}

// webhookHandler redeploys every app built from the pushed repo when the
// push is to its default branch.
func webhookHandler(s *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		app, err := s.GetGitHubApp(r.Context())
		if err != nil {
			http.Error(w, "github app not connected", http.StatusServiceUnavailable)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 25<<20))
		if err != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}
		mac := hmac.New(sha256.New, []byte(app.WebhookSecret))
		mac.Write(body)
		if !hmac.Equal([]byte("sha256="+hex.EncodeToString(mac.Sum(nil))), []byte(r.Header.Get("X-Hub-Signature-256"))) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("X-GitHub-Event") != "push" {
			return
		}

		var push struct {
			Ref        string `json:"ref"`
			Repository struct {
				FullName      string `json:"full_name"`
				DefaultBranch string `json:"default_branch"`
				PushedAt      int64  `json:"pushed_at"` // Unix time, in push events
			} `json:"repository"`
		}
		if err := json.Unmarshal(body, &push); err != nil {
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		if push.Ref != "refs/heads/"+push.Repository.DefaultBranch {
			return
		}
		// The signature doesn't expire: a delivery someone captured would
		// deploy again whenever it's sent. Each is handled once, and an old
		// push not at all.
		if push.Repository.PushedAt > 0 && time.Since(time.Unix(push.Repository.PushedAt, 0)) > store.WebhookMaxAge {
			http.Error(w, "push too old", http.StatusConflict)
			return
		}
		id := r.Header.Get("X-GitHub-Delivery")
		if id == "" {
			http.Error(w, "no delivery ID", http.StatusBadRequest)
			return
		}
		if n, err := s.NoteWebhookDelivery(r.Context(), id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		} else if n == 0 {
			http.Error(w, "delivery handled before", http.StatusConflict)
			return
		}
		apps, err := s.ListAppsByRepo(r.Context(), push.Repository.FullName)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, a := range apps {
			if err := ops.StartDeploy(s, a.Name, "push"); err != nil {
				fmt.Println("push deploy of", a.Name, "skipped:", err)
			}
		}
	}
}
