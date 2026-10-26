package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/hakobudev/hakobu/internal/apply"
	"github.com/hakobudev/hakobu/internal/backups"
	"github.com/hakobudev/hakobu/internal/client"
	"github.com/hakobudev/hakobu/internal/server"
	"github.com/hakobudev/hakobu/internal/update"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

const mcpInstructions = `hakobu deploys projects to servers the user owns. A project is a repository with hakobu.toml (its apps, databases, caches, volumes) and hakobu.secrets.toml (its secrets, sealed). CI builds a release image from a commit; the project's server polls for it and deploys it, and sets the commit's status hakobu/deploy.

To change what runs, edit hakobu.toml in the repository and push: never change a server by hand. Check it with hakobu validate. Secrets go in with hakobu secret set APP NAME (the value on stdin); you can add them but not read them.

These tools see the servers and act on them: status, deploys, logs and backups to look; deploy to check for a release now; rollback to go back one release; backup to back up now; update to update hakobu itself when status shows a newer release (it installs only newer releases signed by hakobu, and puts the old one back if the new one doesn't come up). Restoring a backup and destroying data aren't here: they're the user's to do, with the admin token.

A deploy never destroys data: a database, cache or volume taken out of hakobu.toml stays (status shows it as an orphan). Migrations run in the app's release command, after a dump; write them so the previous version still works with the new schema, as a rollback doesn't undo them.`

func mcpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Serve the servers to an agent over MCP (stdio), with the operator tokens: it can look, deploy, roll back, back up and update hakobu, never restore or destroy",
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
	mcp.AddTool(s, &mcp.Tool{Name: "settings", Description: "Each server's settings: the release images it deploys, registries, the backups' buckets, mail, the pulse and hakobu's updates; never a password or key. The user changes them with hakobu server set.", Annotations: readOnly},
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
