package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/hakobudev/hakobu/internal/apply"
	"github.com/hakobudev/hakobu/internal/backups"
	"github.com/hakobudev/hakobu/internal/client"
	"github.com/hakobudev/hakobu/internal/scaffold"
	"github.com/hakobudev/hakobu/internal/seal"
	"github.com/hakobudev/hakobu/internal/server"
	"github.com/hakobudev/hakobu/internal/spec"
	"github.com/hakobudev/hakobu/internal/update"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

const mcpInstructions = `hakobu deploys projects to servers the user owns. A project is a repository with hakobu.toml (its apps, databases, caches, volumes) and hakobu.secrets.toml (its secrets, sealed). CI builds a release image from a commit; the project's server polls for it and deploys it, and sets the commit's status hakobu/deploy.

To change what runs, edit hakobu.toml in the repository and push: never change a server by hand. Check it with the validate tool. A new repository: the init tool gives the files to write and registers its release on the server.

Secrets: you can add them but never read them, and you never see their values. Ask the user to run, in their own terminal, ssh -t SERVER secret set PROJECT APP NAME, where SERVER is what this MCP server's own ssh command connects to (the value is typed there, unseen, and sealed on the server); then the pending_secrets tool gives the sealed lines, which you write into hakobu.secrets.toml under [apps.APP] (APP hakobu: [hakobu]) and push, and clear. With hakobu on the user's laptop, hakobu secret set APP NAME seals one straight into the file.

These tools see the servers and act on them: status, deploys, logs and backups to look; deploy to check for a release now; rollback to go back one release; backup to back up now; update to update hakobu itself when status shows a newer release (it installs only newer releases signed by hakobu, and puts the old one back if the new one doesn't come up). Restoring a backup, destroying data and the backups' buckets aren't here: they're the user's to do, as root on the server.

A deploy never destroys data: a database, cache or volume taken out of hakobu.toml stays (status shows it as an orphan). Migrations run in the app's release command, after a dump; write them so the previous version still works with the new schema, as a rollback doesn't undo them.`

func mcpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Serve the servers to an agent over MCP (stdio), as the operator: it can look, deploy, roll back, back up and update hakobu, never restore or destroy; on a server (ssh … mcp), that server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return newMCP(cmd).Run(cmd.Context(), &mcp.StdioTransport{})
		},
	}
}

func newMCP(cmd *cobra.Command) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "hakobu", Title: "Hakobu"}, &mcp.ServerOptions{Instructions: mcpInstructions})
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: new(false)}
	acts := &mcp.ToolAnnotations{DestructiveHint: new(false), OpenWorldHint: new(false)}

	type none struct{}
	type statuses struct {
		Servers []server.Status `json:"servers"`
		Errors  []string        `json:"errors,omitempty"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "status", Description: "Every server: its apps (image live, previous one), databases, caches and volumes (orphans are out of hakobu.toml, kept until the user destroys them), the last deploy, and how the last backups went. Start here.", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ none) (*mcp.CallToolResult, statuses, error) {
			var out statuses
			cs, err := clients(cmd)
			if err != nil {
				return nil, out, err
			}
			for _, c := range cs {
				var st server.Status
				if err := c.Get(ctx, "/v1/status", &st); err != nil {
					out.Errors = append(out.Errors, err.Error())
					continue
				}
				out.Servers = append(out.Servers, st)
			}
			return nil, out, nil
		})

	type deploysIn struct {
		Server string `json:"server,omitempty" jsonschema:"the server; all of them if empty"`
		N      int    `json:"n,omitempty" jsonschema:"how many of each server's last deploys; default 10"`
	}
	type deploysOut struct {
		Deploys map[string][]apply.Result `json:"deploys"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "deploys", Description: "The last deploys of each server: the commit, what changed, and why one failed.", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in deploysIn) (*mcp.CallToolResult, deploysOut, error) {
			out := deploysOut{Deploys: map[string][]apply.Result{}}
			cs, err := pick(cmd, in.Server)
			if err != nil {
				return nil, out, err
			}
			n := in.N
			if n <= 0 {
				n = 10
			}
			for _, c := range cs {
				var rs []apply.Result
				if err := c.Get(ctx, fmt.Sprintf("/v1/deploys?n=%d", n), &rs); err != nil {
					return nil, out, err
				}
				out.Deploys[c.Name] = rs
			}
			return nil, out, nil
		})

	type logsIn struct {
		App    string `json:"app" jsonschema:"the app as project/app"`
		Worker bool   `json:"worker,omitempty" jsonschema:"the app's worker instead"`
		Lines  int    `json:"lines,omitempty" jsonschema:"how many lines, up to 5000; default 200"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "logs", Description: "The last lines an app's live container (or its worker) wrote. Errors with their stack traces are in Sentry, if the app reports to it.", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in logsIn) (*mcp.CallToolResult, map[string]string, error) {
			project, app, ok := cutApp(in.App)
			if !ok {
				return nil, nil, errors.New("give the app as project/app")
			}
			c, err := clientFor(cmd, project)
			if err != nil {
				return nil, nil, err
			}
			q := fmt.Sprintf("/v1/logs?project=%s&app=%s&lines=%d", project, app, in.Lines)
			if in.Worker {
				q += "&worker=1"
			}
			var out map[string]string
			return nil, out, c.Get(ctx, q, &out)
		})

	type projectIn struct {
		Project string `json:"project" jsonschema:"the project, as hakobu.toml names it"`
	}
	type backupsOut struct {
		Backups []backups.Manifest `json:"backups"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "backups", Description: "A project's backups, newest first: each database and volume's, when, its size and what reading it back found.", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in projectIn) (*mcp.CallToolResult, backupsOut, error) {
			var out backupsOut
			c, err := clientFor(cmd, in.Project)
			if err != nil {
				return nil, out, err
			}
			return nil, out, c.Get(ctx, "/v1/backups?project="+in.Project, &out.Backups)
		})

	type deployIn struct {
		Project string `json:"project" jsonschema:"the project"`
		Force   bool   `json:"force,omitempty" jsonschema:"deploy the current release again, even one that failed"`
	}
	type results struct {
		Results []map[string]any `json:"results"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "deploy", Description: "Check the project's release image now instead of at the next poll (every 30 s), and deploy it if it's new; force deploys it again. Returns the deploy's log; a failed one leaves the live version serving.", Annotations: acts},
		func(ctx context.Context, _ *mcp.CallToolRequest, in deployIn) (*mcp.CallToolResult, results, error) {
			var out results
			c, err := clientFor(cmd, in.Project)
			if err != nil {
				return nil, out, err
			}
			return nil, out, c.Post(ctx, "/v1/deploy", map[string]any{"project": in.Project, "force": in.Force}, &out.Results)
		})
	mcp.AddTool(s, &mcp.Tool{Name: "rollback", Description: "Deploy the project's release before the current one (code and hakobu.toml; secrets stay current) and hold the current one back until a newer release comes. Database migrations aren't undone.", Annotations: acts},
		func(ctx context.Context, _ *mcp.CallToolRequest, in projectIn) (*mcp.CallToolResult, map[string]any, error) {
			c, err := clientFor(cmd, in.Project)
			if err != nil {
				return nil, nil, err
			}
			var out map[string]any
			return nil, out, c.Post(ctx, "/v1/rollback", map[string]string{"project": in.Project}, &out)
		})
	type serverIn struct {
		Server string `json:"server,omitempty" jsonschema:"the server; all of them if empty"`
	}
	type runs struct {
		Runs map[string]server.BackupRun `json:"runs"`
	}
	type settingsOut struct {
		Settings map[string]server.SettingsView `json:"settings"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "settings", Description: "Each server's settings: the release images it deploys, registries, the backups' buckets, mail, the pulse and hakobu's updates; never a password or key. The user changes them with ssh SERVER server set (hakobu server set on a laptop).", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in serverIn) (*mcp.CallToolResult, settingsOut, error) {
			out := settingsOut{Settings: map[string]server.SettingsView{}}
			cs, err := pick(cmd, in.Server)
			if err != nil {
				return nil, out, err
			}
			for _, c := range cs {
				var v server.SettingsView
				if err := c.Get(ctx, "/v1/settings", &v); err != nil {
					return nil, out, err
				}
				out.Settings[c.Name] = v
			}
			return nil, out, nil
		})
	type updateIn struct {
		Server string `json:"server,omitempty" jsonschema:"the server; all of them if empty"`
		To     string `json:"to,omitempty" jsonschema:"a release, like v1.3.0; the latest if empty"`
	}
	type updates struct {
		Updates map[string]update.Status `json:"updates"`
		Errors  []string                 `json:"errors,omitempty"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "update", Description: "Update hakobu on a server to a newer release (status shows one as latest), and wait for how it went: only releases signed by hakobu, only newer ones; if the new version doesn't come up within minutes, the one before goes back. Apps keep running; their proxies pause for about a second.", Annotations: acts},
		func(ctx context.Context, _ *mcp.CallToolRequest, in updateIn) (*mcp.CallToolResult, updates, error) {
			out := updates{Updates: map[string]update.Status{}}
			cs, err := pick(cmd, in.Server)
			if err != nil {
				return nil, out, err
			}
			to := in.To
			if to != "" {
				to = update.Tag(to)
			}
			for _, c := range cs {
				st, err := updateServer(ctx, c, to, io.Discard)
				if err != nil {
					out.Errors = append(out.Errors, c.Name+": "+err.Error())
				}
				if st.State != "" {
					out.Updates[c.Name] = st
				}
			}
			return nil, out, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "backup", Description: "Back up every database and volume of a server now, as the daily backups do, each read back before it counts. Takes a while for big databases.", Annotations: acts},
		func(ctx context.Context, _ *mcp.CallToolRequest, in serverIn) (*mcp.CallToolResult, runs, error) {
			out := runs{Runs: map[string]server.BackupRun{}}
			cs, err := pick(cmd, in.Server)
			if err != nil {
				return nil, out, err
			}
			for _, c := range cs {
				var run server.BackupRun
				if err := c.Post(ctx, "/v1/backup", struct{}{}, &run); err != nil {
					return nil, out, err
				}
				out.Runs[c.Name] = run
			}
			return nil, out, nil
		})
	type pendingIn struct {
		Project string `json:"project" jsonschema:"the project"`
		Clear   bool   `json:"clear,omitempty" jsonschema:"forget them: they're in hakobu.secrets.toml now"`
	}
	type pendingOut struct {
		Secrets []server.PendingSecret `json:"secrets"`
		Note    string                 `json:"note"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "pending_secrets", Description: "The secrets the user sealed on the server (ssh -t SERVER secret set PROJECT APP NAME) for this project, each a sealed value to write into hakobu.secrets.toml as NAME = \"age:…\" under [apps.APP], or [hakobu] for APP hakobu; then push, and call this again with clear. A value can't be read here or anywhere but the server.", Annotations: acts},
		func(ctx context.Context, _ *mcp.CallToolRequest, in pendingIn) (*mcp.CallToolResult, pendingOut, error) {
			out := pendingOut{Note: "Write each into hakobu.secrets.toml, keeping its recipients; replace a value that's there. Then clear."}
			c, err := clientFor(cmd, in.Project)
			if err != nil {
				return nil, out, err
			}
			if in.Clear {
				out.Note = "Forgotten on the server."
				return nil, out, c.Post(ctx, "/v1/secrets/clear?project="+in.Project, struct{}{}, &out.Secrets)
			}
			return nil, out, c.Get(ctx, "/v1/secrets?project="+in.Project, &out.Secrets)
		})

	type initIn struct {
		Server  string `json:"server,omitempty" jsonschema:"the server to deploy to; the only one if empty"`
		Project string `json:"project" jsonschema:"the project's name: lowercase letters, digits, -"`
		Image   string `json:"image" jsonschema:"the release image without a tag, like ghcr.io/owner/repo (the GitHub repository, lowercase)"`
		Branch  string `json:"branch,omitempty" jsonschema:"the branch whose pushes deploy; default main"`
	}
	type initOut struct {
		Files map[string]string `json:"files"`
		Next  string            `json:"next"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "init", Description: "Set a repository up to deploy: the files to write (hakobu.toml with one app to fill in, hakobu.secrets.toml sealed to this server, the CI workflow, the skill; CLAUDE.md is a section to add, .claude/settings.json keys to merge), and the release registered on the server, which deploys it from the first push.", Annotations: acts},
		func(ctx context.Context, _ *mcp.CallToolRequest, in initIn) (*mcp.CallToolResult, initOut, error) {
			var out initOut
			cs, err := pick(cmd, in.Server)
			if err != nil {
				return nil, out, err
			}
			if len(cs) != 1 {
				return nil, out, errors.New("which server? give its name")
			}
			out.Files, err = scaffold.Bundle(scaffold.Options{Server: cs[0].Name, Project: in.Project, Image: in.Image, Branch: in.Branch, Version: cmd.Root().Version})
			if err != nil {
				return nil, out, err
			}
			var key map[string]string
			if err := cs[0].Get(ctx, "/v1/key", &key); err != nil {
				return nil, out, err
			}
			sec, err := seal.New(key["recipient"], key["owner"])
			if err != nil {
				return nil, out, err
			}
			b, err := sec.Marshal()
			if err != nil {
				return nil, out, err
			}
			out.Files[seal.FileName] = string(b)
			if err := cs[0].Post(ctx, "/v1/settings", server.SettingsChange{AddRelease: in.Image + ":main"}, nil); err != nil {
				return nil, out, err
			}
			out.Next = "Write the files (keep any that are there but CLAUDE.md's section and the skill). In hakobu.toml, add each app with its Dockerfile's directory, domain, port and health path, and its databases; check it with validate. The status token: a fine-grained GitHub token with Commit statuses: write on this repository alone, which the user seals with ssh -t SERVER secret set " + in.Project + " hakobu status_token; then pending_secrets. Push; while the first job waits, the user makes the package " + in.Image[strings.LastIndex(in.Image, "/")+1:] + " public on GitHub (Package settings, Change visibility): it's sealed, and the server reads it with no token."
			return nil, out, nil
		})

	type validateIn struct {
		Server  string `json:"server,omitempty" jsonschema:"the server it deploys to; the only one if empty"`
		Spec    string `json:"hakobu_toml" jsonschema:"hakobu.toml's text"`
		Secrets string `json:"hakobu_secrets_toml,omitempty" jsonschema:"hakobu.secrets.toml's text, if there's one"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "validate", Description: "Check hakobu.toml (and hakobu.secrets.toml) as CI and the server will: strict, an unknown key or a reference to nothing is an error; and that the secrets are sealed to this server's key.", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in validateIn) (*mcp.CallToolResult, map[string]string, error) {
			p, err := spec.Parse([]byte(in.Spec))
			if err != nil {
				return nil, nil, fmt.Errorf("hakobu.toml: %w", err)
			}
			if in.Secrets != "" {
				sec, err := seal.Parse([]byte(in.Secrets))
				if err == nil {
					err = sec.CheckApps(slices.Sorted(maps.Keys(p.Apps)))
				}
				if err != nil {
					return nil, nil, fmt.Errorf("%s: %w", seal.FileName, err)
				}
				cs, err := pick(cmd, in.Server)
				if err == nil && len(cs) == 1 {
					var key map[string]string
					if cs[0].Get(ctx, "/v1/key", &key) == nil && !slices.Contains(sec.Recipients, key["recipient"]) {
						return nil, nil, fmt.Errorf("%s isn't sealed to %s's key %s: that server can't open it", seal.FileName, cs[0].Name, key["recipient"])
					}
				}
			}
			return nil, map[string]string{"result": "ok"}, nil
		})
	return s
}

// pick is the named server, or all of them for "".
func pick(cmd *cobra.Command, name string) ([]*client.Client, error) {
	if name != "" {
		if err := cmd.Flags().Set("server", name); err != nil {
			return nil, err
		}
		defer func() { _ = cmd.Flags().Set("server", "") }()
	}
	return clients(cmd)
}

func cutApp(s string) (project, app string, ok bool) {
	for i := range len(s) {
		if s[i] == '/' {
			return s[:i], s[i+1:], i > 0 && i < len(s)-1
		}
	}
	return "", "", false
}
