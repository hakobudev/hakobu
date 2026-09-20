package cmd

import (
	"bytes"
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
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/x0ryz/hako/internal/build"
	"github.com/x0ryz/hako/internal/config"
	"github.com/x0ryz/hako/internal/edge"
	"github.com/x0ryz/hako/internal/github"
	"github.com/x0ryz/hako/internal/ops"
	"github.com/x0ryz/hako/internal/store"
)

var agentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Run the deploy agent daemon",
	RunE:  runAgent,
}

func init() {
	agentCmd.Flags().StringVar(&agentAddr, "addr", "", "listen address (default 127.0.0.1:9000, or HAKO_ADDR)")
	agentCmd.Flags().StringVar(&agentPublicHost, "public-host", "", "public host for SENTRY_DSN + webhook URL (or HAKO_PUBLIC_HOST)")
}

var (
	agentAddr       string
	agentPublicHost string
)

var agentStartedAt = time.Now()

func runAgent(cmd *cobra.Command, args []string) error {
	s, err := store.Open("data/hako.db")
	if err != nil {
		return err
	}

	apiToken, err := s.GetOrCreateAPIToken(func() (string, error) { return ops.RandomHex(32) })
	if err != nil {
		return err
	}

	if err := configureR2FromEnv(s); err != nil {
		return fmt.Errorf("failed to save R2 config from environment: %w", err)
	}

	addr := agentAddr
	if addr == "" {
		addr = config.AgentAddr
	}
	if agentPublicHost != "" {
		if err := config.SetPublicHost(agentPublicHost); err != nil {
			return fmt.Errorf("failed to save public host: %w", err)
		}
	}

	mux := http.NewServeMux()
	if app, err := s.GetGitHubApp(); err != nil {
		fmt.Println("warning: no GitHub App connected, skipping /webhook/github (connect it via web UI)")
	} else {
		mux.HandleFunc("/webhook/github", makeWebhookHandler(s, app.AppID, app.PrivateKey, app.WebhookSecret))
	}
	registerAPIRoutes(mux, s, apiToken)
	registerWebRoutes(mux, s, apiToken)
	registerIngestRoutes(mux, s)

	// Bind BEFORE printing anything: a stale process on the same port used
	// to print a token and a fake "listening" line before failing.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w\n(hint: another hako agent may already be running — check `ss -tlnp | grep 9000`)", addr, err)
	}

	go runHealthPoller(s)
	go runBackupScheduler(s)
	edge.Start(s, addr)

	fmt.Println("Agent listening on", ln.Addr())
	fmt.Println("Setup wizard: open http://" + ln.Addr().String() + "/setup (first run, no token needed)")
	fmt.Println("Dashboard: open the agent's URL in a browser and sign in with this API token:", apiToken)
	srv := &http.Server{
		Handler:      mux,
		ReadTimeout:  config.ReadTimeout,
		WriteTimeout: config.WriteTimeout,
		IdleTimeout:  config.IdleTimeout,
	}
	return srv.Serve(ln)
}

// runHealthPoller periodically checks every project/database and records the
// result, so the dashboard's uptime bars have real history instead of just
// a single point-in-time check made when the page happens to load.
func runHealthPoller(s *store.Store) {
	ticker := time.NewTicker(config.HealthPollEvery)
	defer ticker.Stop()

	check := func() {
		if projects, err := s.ListProjects(); err == nil {
			for _, p := range projects {
				ops.EnsureProjectProxy(p)
				ops.CheckProjectHealth(s, p)
				ops.SampleResources(s, "project:"+p.Name, p.ContainerName())
				if worker, err := s.GetWorker(p.Name); err == nil {
					ops.CheckWorkerHealth(s, p.Name)
					ops.SampleResources(s, "worker:"+p.Name, worker.ContainerName())
				}
			}
		}
		if databases, err := s.ListDatabases(); err == nil {
			for _, d := range databases {
				ops.CheckDatabaseHealth(s, d)
				ops.SampleResources(s, "database:"+d.Name, d.ContainerName)
			}
		}
		ops.SampleHostAndAgent(s)
	}

	check()
	for range ticker.C {
		check()
	}
}

// envStorageName is the name given to a storage entry synced in from
// R2_* env vars — a plain constant since there's only ever one env-synced
// entry. It still has to be picked as a database's backup storage
// explicitly (per database, on that database's Backups panel) — env vars
// only save retyping the credentials, not the per-database choice.
const envStorageName = "r2-env"

// configureR2FromEnv syncs R2_* env vars into a storage entry named
// unit, .env, or process manager config) and the agent creates/updates a
// storage entry named "r2-env" on every startup, so restarting the agent
// after an env change is enough; no extra step needed. A no-op if any of the
// four are unset, so an existing config set another way isn't wiped out by
// a partially configured environment.
func configureR2FromEnv(s *store.Store) error {
	accountID := os.Getenv("R2_ACCOUNT_ID")
	accessKeyID := os.Getenv("R2_ACCESS_KEY_ID")
	secretAccessKey := os.Getenv("R2_SECRET_ACCESS_KEY")
	bucket := os.Getenv("R2_BUCKET_NAME")

	if accountID == "" || accessKeyID == "" || secretAccessKey == "" || bucket == "" {
		return nil
	}

	return s.SaveStorage(store.Storage{
		Name:            envStorageName,
		Provider:        "r2",
		AccountID:       accountID,
		AccessKeyID:     accessKeyID,
		SecretAccessKey: secretAccessKey,
		Bucket:          bucket,
		Region:          "auto",
	})
}

// runBackupScheduler backs up every database that has a backup storage
// picked, once a day. It deliberately doesn't back up immediately on
// startup: an agent restart during normal operation (a redeploy, a crash)
// shouldn't trigger an extra backup cycle on top of the daily one.
func runBackupScheduler(s *store.Store) {
	ticker := time.NewTicker(config.BackupEvery)
	defer ticker.Stop()

	for range ticker.C {
		for name, err := range ops.BackupAllDatabases(s) {
			if err != nil {
				fmt.Println("scheduled backup failed for", name, ":", err)
			}
		}
		if err := s.PruneOldData(config.RetentionDays); err != nil {
			fmt.Println("retention prune failed:", err)
		}
	}
}

func makeWebhookHandler(s *store.Store, appID int64, privateKey, webhookSecret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}

		signature := r.Header.Get("X-Hub-Signature-256")
		if !verifySignature(webhookSecret, body, signature) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}

		event := r.Header.Get("X-GitHub-Event")
		if event != "push" {
			w.WriteHeader(http.StatusOK)
			return
		}

		var payload struct {
			Ref        string `json:"ref"`
			Repository struct {
				FullName string `json:"full_name"`
				CloneURL string `json:"clone_url"`
			} `json:"repository"`
			Installation struct {
				ID int64 `json:"id"`
			} `json:"installation"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}

		fmt.Printf("push received: repo=%s ref=%s installation=%d\n",
			payload.Repository.FullName, payload.Ref, payload.Installation.ID)

		w.WriteHeader(http.StatusOK)

		go func() {
			var log bytes.Buffer
			out := io.MultiWriter(&log, os.Stdout)

			projectName := payload.Repository.FullName
			fail := func(step string, err error) {
				fmt.Fprintln(out, step+": "+err.Error())
				if project, lookupErr := s.GetProjectByRepo(payload.Repository.FullName); lookupErr == nil {
					projectName = project.Name
				}
				s.CreateDeployLog(store.DeployLog{Project: projectName, Trigger: "push", Status: "failed", Output: log.String()})
			}

			token, err := github.GetInstallationToken(appID, privateKey, payload.Installation.ID)
			if err != nil {
				fail("failed to get installation token", err)
				return
			}

			project, err := s.GetProjectByRepo(payload.Repository.FullName)
			if err != nil {
				fmt.Println("no matching project for repo:", payload.Repository.FullName)
				return
			}
			projectName = project.Name

			repoName := strings.ReplaceAll(payload.Repository.FullName, "/", "-")
			workDir := "data/work/" + repoName

			fmt.Fprintln(out, "cloning", payload.Repository.FullName)
			if err := build.CloneRepo(payload.Repository.CloneURL, token, workDir, out); err != nil {
				fail("clone failed", err)
				return
			}

		buildDir, err := ops.SafeBuildDir(workDir, project.BuildPath)
		if err != nil {
			fail("invalid build path", err)
			return
		}

			imageTag := ops.ImageTag(*project)
			if err := ops.SnapshotPreviousImage(context.Background(), *project); err != nil {
				fmt.Fprintln(out, "warning: failed to snapshot previous image for rollback:", err)
			}
			fmt.Fprintln(out, "building", imageTag, "from", buildDir, "strategy:", project.BuildStrategy)
			if err := build.BuildWithStrategy(buildDir, imageTag, project.BuildStrategy, out); err != nil {
				fail("build failed", err)
				return
			}

			fmt.Fprintln(out, "build succeeded:", imageTag)

			env, err := ops.AppEnv(s, *project)
			if err != nil {
				fail("failed to build env", err)
				return
			}

			if _, err := ops.DeployImage(s, *project, imageTag, env, out); err != nil {
				fail("deploy failed", err)
				return
			}

			// The worker shares this same image/repo, so a fresh app build
			// means its code changed too — roll it forward right along with
			// the app instead of leaving it on stale code until someone
			// notices and redeploys it by hand.
			if _, err := s.GetWorker(project.Name); err == nil {
				fmt.Fprintln(out, "redeploying worker with the new image")
				if _, err := ops.RedeployWorker(s, project.Name); err != nil {
					fmt.Fprintln(out, "worker redeploy failed:", err)
				}
			}

			s.CreateDeployLog(store.DeployLog{Project: projectName, Trigger: "push", Status: "success", Output: log.String()})
		}()
	}
}

func verifySignature(secret string, body []byte, signatureHeader string) bool {
	if signatureHeader == "" {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(expected), []byte(signatureHeader))
}
