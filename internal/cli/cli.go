// Package cli is hakobu v1's command line: on a server it runs the daemon;
// on a laptop it talks to the servers and edits a project's files.
package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/hakobudev/hakobu/internal/apply"
	"github.com/hakobudev/hakobu/internal/backups"
	"github.com/hakobudev/hakobu/internal/client"
	"github.com/hakobudev/hakobu/internal/deploy"
	"github.com/hakobudev/hakobu/internal/node"
	"github.com/hakobudev/hakobu/internal/scaffold"
	"github.com/hakobudev/hakobu/internal/seal"
	"github.com/hakobudev/hakobu/internal/server"
	"github.com/hakobudev/hakobu/internal/spec"
	"github.com/hakobudev/hakobu/internal/update"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// Root is the command line; version is the build's.
func Root(version string) *cobra.Command {
	root := &cobra.Command{
		Use:     "hakobu",
		Short:   "Deploy projects to servers you own, from their hakobu.toml",
		Version: version,
		// Usage is for mistakes in the command line; once a command
		// runs, its errors stand alone.
		PersistentPreRun: func(cmd *cobra.Command, args []string) { cmd.SilenceUsage = true },
		SilenceErrors:    true,
	}
	root.PersistentFlags().String("server", "", "the server to talk to (default: the one the project is on, or the only one)")
	root.AddCommand(serveCmd(version), serverCmd(version), statusCmd(), deploysCmd(), logsCmd(), deployCmd(), rollbackCmd(),
		backupCmd(), backupsCmd(), restoreCmd(), destroyCmd(), initCmd(), secretCmd(), validateCmd(), buildCmd(), waitCmd(),
		mcpCmd(), dialerCmd(), sshCmd(version))
	return root
}

// On the server.

func serveCmd(version string) *cobra.Command {
	config := server.ConfigFile
	c := &cobra.Command{
		Use:   "serve",
		Short: "Run hakobu on this server (its systemd unit does)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := server.LoadConfig(config)
			if err != nil {
				return fmt.Errorf("%s: %w", config, err)
			}
			d, err := server.New(cfg, node.Local{}, os.Stderr)
			if err != nil {
				return err
			}
			d.Version, d.LatestRelease = version, update.LatestRelease
			if bin, err := os.Executable(); err == nil {
				d.InstallDir = filepath.Dir(bin)
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			err = d.Run(ctx)
			if errors.Is(err, server.ErrRestart) {
				// In place, the same process: systemd would wait
				// RestartSec, with the apps' proxies down.
				bin, berr := os.Executable()
				if berr != nil {
					return berr
				}
				return syscall.Exec(bin, os.Args, os.Environ())
			}
			return err
		},
	}
	c.Flags().StringVar(&config, "config", config, "the server's config")
	return c
}

// InstallScript is install.sh, which main embeds: an update runs the new
// release's own.
var InstallScript string

func serverCmd(version string) *cobra.Command {
	c := &cobra.Command{Use: "server", Short: "Set up this server, or the servers this laptop knows"}
	config := server.ConfigFile
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Make this server's key, if it has none, and print its public half",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := server.LoadConfig(config)
			if err != nil {
				return fmt.Errorf("%s: %w", config, err)
			}
			pub, err := server.InitKey(cfg)
			if err != nil {
				return err
			}
			fmt.Println(pub)
			fmt.Fprintln(os.Stderr, "Back up", cfg.KeyFile(), "somewhere off this server: without it (or the owner's key), its backups and secrets can't be opened.")
			return nil
		},
	}
	initCmd.Flags().StringVar(&config, "config", config, "the server's config")
	token := &cobra.Command{
		Use:   "token",
		Short: "Make an operator and an admin token, and print them with the lines for the server's config",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			op, admin := newToken(), newToken()
			fmt.Printf("operator token: %s\nadmin token:    %s\n\n", op, admin)
			fmt.Printf("In %s:\noperator_token_sha256 = %q\nadmin_token_sha256 = %q\n\n", server.ConfigFile, server.HashToken(op), server.HashToken(admin))
			fmt.Println("Keep the admin token in your password manager, not on a disk an agent can read; the operator one goes in hakobu server add.")
			return nil
		},
	}
	add := &cobra.Command{
		Use:   "add NAME URL",
		Short: "Remember a server hakobu server create didn't make: its API's URL (on the tailnet, or through your own ssh -L) and the operator token (read from stdin)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			tok, err := readSecret("operator token: ")
			if err != nil {
				return err
			}
			file, all, err := servers()
			if err != nil {
				return err
			}
			all[args[0]] = client.Server{URL: args[1], Token: tok}
			cl, _ := all.Client(args[0])
			var key map[string]string
			if err := cl.Get(cmd.Context(), "/v1/key", &key); err != nil {
				return err
			}
			if key["server"] != args[0] {
				return fmt.Errorf("that's server %s, not %s", key["server"], args[0])
			}
			if err := all.Save(file); err != nil {
				return err
			}
			fmt.Printf("added %s; its key: %s\n", args[0], key["recipient"])
			return nil
		},
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List the servers this laptop knows",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, all, err := servers()
			if err != nil {
				return err
			}
			for _, n := range all.Names() {
				fmt.Printf("%s\t%s\n", n, all[n].URL)
			}
			return nil
		},
	}
	updateCmd := serverUpdateCmd(version, &config)
	installer := &cobra.Command{
		Use:    "installer",
		Short:  "Print this release's install.sh (an update runs it)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := io.WriteString(os.Stdout, InstallScript)
			return err
		},
	}
	c.AddCommand(serverCreateCmd(version), initCmd, token, add, list, serverGetCmd(), serverSetCmd(), updateCmd, installer, ownerKeyCmd())
	return c
}

func newToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "hk_" + hex.EncodeToString(b)
}

// dialerCmd serves hakobu's connections to containers: the Docker user's
// unit runs it inside rootless Docker's namespaces.
func dialerCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "dialer SOCKET",
		Short:  "Serve hakobu's connections to containers (run by the Docker user)",
		Args:   cobra.ExactArgs(1),
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return deploy.ServeDialerSocket(args[0])
		},
	}
}

// On the laptop.

func servers() (string, client.Servers, error) {
	file, err := client.ServersFile()
	if err != nil {
		return "", nil, err
	}
	all, err := client.LoadServers(file)
	return file, all, err
}

// localSocket is the API's socket on a server; a variable for the tests.
var localSocket = server.Socket

// known are the servers to talk to: the laptop's, or on a server (no
// servers.toml there) the server itself, through its socket.
func known() (client.Servers, error) {
	_, all, err := servers()
	if err != nil || len(all) > 0 {
		return all, err
	}
	if name, sv, ok := client.Local(localSocket); ok {
		all[name] = sv
	}
	return all, nil
}

// printable is an app's text without the control characters a terminal
// would act on (escape sequences can retitle it, write to the clipboard,
// or draw a prompt that isn't there); colors go with them.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, s)
}

// clients are the servers to ask: --server's, or all of them.
func clients(cmd *cobra.Command) ([]*client.Client, error) {
	name, _ := cmd.Flags().GetString("server")
	return clientsNamed(name)
}

// clientsNamed is the named server, or all of them for "".
func clientsNamed(name string) ([]*client.Client, error) {
	all, err := known()
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		if _, err := os.Stat(filepath.Dir(localSocket)); err == nil {
			return nil, errors.New("hakobu on this server doesn't answer yet: it may be starting (systemctl status hakobu, journalctl -u hakobu)")
		}
		return nil, errors.New("no servers yet: hakobu server create NAME --ssh root@HOST")
	}
	names := all.Names()
	if name != "" {
		names = []string{name}
	}
	var out []*client.Client
	for _, n := range names {
		c, err := all.Client(n)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// clientFor is the server the project runs on: --server's, the one named
// in ./hakobu.toml if it's this project's, or the one whose status has it.
func clientFor(cmd *cobra.Command, project string) (*client.Client, error) {
	cs, err := clients(cmd)
	if err != nil {
		return nil, err
	}
	if len(cs) == 1 {
		return cs[0], nil
	}
	if b, err := os.ReadFile(spec.File); err == nil {
		if p, err := spec.Parse(b); err == nil && p.Name == project {
			for _, c := range cs {
				if c.Name == p.Server {
					return c, nil
				}
			}
		}
	}
	for _, c := range cs {
		var st server.Status
		if c.Get(cmd.Context(), "/v1/status", &st) == nil && st.Apps[project] != nil {
			return c, nil
		}
	}
	return nil, fmt.Errorf("no server has project %s; pick one with --server", project)
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show every server: its apps, data, last deploy and last backups",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cs, err := clients(cmd)
			if err != nil {
				return err
			}
			var errs []error
			for i, c := range cs {
				if i > 0 {
					fmt.Println()
				}
				var st server.Status
				if err := c.Get(cmd.Context(), "/v1/status", &st); err != nil {
					fmt.Printf("%s: unreachable\n", c.Name)
					errs = append(errs, err)
					continue
				}
				printStatus(os.Stdout, st)
			}
			return errors.Join(errs...)
		},
	}
}

// printStatus is a server at a glance: what's wrong first, then each
// project's apps and data, then how full the machine is.
func printStatus(w io.Writer, st server.Status) {
	l := lookFor(w)
	down := map[string]bool{}
	for _, a := range st.Down {
		down[a] = true
	}

	// What's wrong, each a subject and its detail.
	type issue struct{ sym, title, detail string }
	var issues []issue
	for _, e := range st.PollErrors {
		issues = append(issues, issue{symBad, "release", printable(e)})
	}
	for _, p := range st.Problems {
		title, detail := split(p.Subject)
		issues = append(issues, issue{symBad, title, strings.TrimPrefix(detail+" · "+since(p.Since), " · ")})
	}
	if u := st.LastUpdate; u != nil && (u.State == "failed" || u.State == "rolled back") && time.Since(u.At) < 7*24*time.Hour {
		issues = append(issues, issue{symBad, "update " + u.State, firstLine(printable(u.Message)) + " · " + since(u.At)})
	}
	bad := len(issues)
	if !st.Mail {
		issues = append(issues, issue{symData, "no mail", "problems show only here: ssh " + st.Server + " server set mail"})
	}
	if !st.Backups {
		issues = append(issues, issue{symData, "no backups", "no bucket: ssh root@" + st.Server + " hakobu server set backups"})
	}

	head := l.bold(st.Server) + "  "
	if bad > 0 {
		plural := "s"
		if bad == 1 {
			plural = ""
		}
		head += l.bad(fmt.Sprintf("%s %d problem%s", symBad, bad, plural))
	} else if len(issues) > 0 {
		head += l.warn(fmt.Sprintf("%s %d to set up", symData, len(issues)))
	} else {
		head += l.ok(symOK + " all good")
	}
	var about []string
	if st.Version != "" {
		about = append(about, "hakobu "+st.Version)
	}
	if h := st.Host; h != nil {
		if h.OS != "" {
			about = append(about, h.OS)
		}
		about = append(about, h.Arch)
		if h.Uptime > 0 {
			about = append(about, "up "+since(time.Now().Add(-time.Duration(h.Uptime)*time.Second)))
		}
	}
	if len(about) > 0 {
		head += "  " + l.dim(strings.Join(about, " · "))
	}
	fmt.Fprintln(w, head)
	if st.Latest != "" {
		fmt.Fprintln(w, l.warn(symUpdate+" "+st.Latest+" is out")+"  "+l.dim("ssh "+st.Server+" server update"))
	}

	if len(issues) > 0 {
		fmt.Fprintln(w)
		titles := make([]string, len(issues))
		for i, x := range issues {
			titles[i] = x.title
		}
		tw := width(titles...)
		for _, x := range issues {
			sym, title := l.bad(x.sym), l.bold(x.title)
			if x.sym == symData {
				sym, title = l.warn(x.sym), l.warn(x.title)
			}
			fmt.Fprintf(w, "  %s %s  %s\n", sym, pad(title, x.title, tw), l.dim(x.detail))
		}
	}

	// Each project: its apps, then its data.
	type row struct{ sym, name, state, detail, hint string }
	rows := map[string][]row{}
	for p, apps := range st.Apps {
		for a, as := range apps {
			image, _, _ := strings.Cut(as.Image, "@")
			r := row{l.ok(symOK), a, "up", l.dim(image), ""}
			if down[p+"/"+a] {
				r.sym, r.state = l.bad(symBad), "down"
			}
			rows[p] = append(rows[p], r)
		}
	}
	slices.SortFunc(st.Resources, func(a, b server.ResourceStatus) int { return strings.Compare(a.Name, b.Name) })
	kinds := map[string]string{"database": "postgres", "cache": "valkey"}
	for _, x := range st.Resources {
		kind := kinds[x.Kind]
		if kind == "" {
			kind = x.Kind
		}
		r := row{l.ok(symData), x.Name, kind, "", ""}
		switch {
		case x.Orphan:
			r.sym, r.detail = l.warn(symData), l.warn("not in hakobu.toml")
			r.hint = "ssh root@" + st.Server + " hakobu destroy " + x.Project + "/" + x.Name
		case st.LastBackup != nil && slices.ContainsFunc(st.LastBackup.Made, func(m string) bool {
			return strings.HasPrefix(m, x.Kind+" "+x.Project+"/"+x.Name+" of ")
		}):
			r.detail = l.dim("backed up " + since(st.LastBackup.At) + " ago")
		}
		rows[x.Project] = append(rows[x.Project], r)
	}
	projects := slices.Sorted(maps.Keys(rows))
	var names, states []string
	for _, p := range projects {
		for _, r := range rows[p] {
			names, states = append(names, r.name), append(states, r.state)
		}
	}
	nw, sw := width(names...), width(states...)
	for _, p := range projects {
		fmt.Fprintln(w)
		line := l.bold(p)
		if d, ok := st.Deployed[p]; ok {
			if d.Error != "" {
				line += "  " + l.bad("deploy failed") + " " + l.dim(shortRev(d.Revision)+" · "+since(d.At)+" ago")
			} else {
				line += "  " + l.dim(strings.TrimSpace("deployed "+shortRev(d.Revision))+" · "+since(d.At)+" ago")
			}
		}
		fmt.Fprintln(w, line)
		// Apps first, as the server lists them; then the data.
		slices.SortStableFunc(rows[p], func(a, b row) int {
			if (a.state == "up" || a.state == "down") != (b.state == "up" || b.state == "down") {
				if a.state == "up" || a.state == "down" {
					return -1
				}
				return 1
			}
			return strings.Compare(a.name, b.name)
		})
		for _, r := range rows[p] {
			state := r.state
			if state == "down" {
				state = l.bad(state)
			}
			fmt.Fprintln(w, strings.TrimRight(fmt.Sprintf("  %s %s  %s  %s", r.sym, pad(r.name, r.name, nw), pad(state, r.state, sw), r.detail), " "))
			if r.hint != "" {
				fmt.Fprintf(w, "    %s  %s  %s\n", strings.Repeat(" ", nw), strings.Repeat(" ", sw), l.dim(r.hint))
			}
		}
	}
	if len(projects) == 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, l.dim("Nothing deployed yet: ask your agent to set a repository up."))
	}

	if h := st.Host; h != nil {
		var meters []string
		add := func(name string, used, total float64, warnAt float64) {
			if total <= 0 {
				return
			}
			share := used / total
			pct := fmt.Sprintf("%3.0f%%", share*100)
			if share >= warnAt {
				pct = l.warn(pct)
			}
			meters = append(meters, l.dim(name)+" "+l.meter(share, share >= warnAt)+" "+pct)
		}
		add("disk", float64(h.DiskUsed), float64(h.DiskTotal), 0.8)
		add("mem", float64(h.MemUsed), float64(h.MemTotal), 0.9)
		add("cpu", h.CPU, 1, 0.9)
		if len(meters) > 0 {
			fmt.Fprintln(w)
			fmt.Fprintln(w, strings.Join(meters, "   "))
		}
	}
}

func deploysCmd() *cobra.Command {
	n := 10
	c := &cobra.Command{
		Use:   "deploys",
		Short: "Show the last deploys on each server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cs, err := clients(cmd)
			if err != nil {
				return err
			}
			for i, c := range cs {
				var rs []apply.Result
				if err := c.Get(cmd.Context(), fmt.Sprintf("/v1/deploys?n=%d", n), &rs); err != nil {
					return err
				}
				if i > 0 {
					fmt.Println()
				}
				printDeploys(os.Stdout, c.Name, rs)
			}
			return nil
		},
	}
	c.Flags().IntVarP(&n, "lines", "n", n, "how many")
	return c
}

func logsCmd() *cobra.Command {
	lines := 200
	worker := false
	c := &cobra.Command{
		Use:   "logs PROJECT/APP",
		Short: "Show an app's latest output, or its worker's",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			project, app, ok := strings.Cut(args[0], "/")
			if !ok {
				return errors.New("name the app as PROJECT/APP")
			}
			cl, err := clientFor(cmd, project)
			if err != nil {
				return err
			}
			var out map[string]string
			q := fmt.Sprintf("/v1/logs?project=%s&app=%s&lines=%d", url.QueryEscape(project), url.QueryEscape(app), lines)
			if worker {
				q += "&worker=1"
			}
			if err := cl.Get(cmd.Context(), q, &out); err != nil {
				return err
			}
			fmt.Print(printable(out["logs"]))
			return nil
		},
	}
	c.Flags().IntVarP(&lines, "lines", "n", lines, "how many lines")
	c.Flags().BoolVar(&worker, "worker", false, "the worker's output")
	return c
}

func deployCmd() *cobra.Command {
	force := false
	c := &cobra.Command{
		Use:   "deploy [PROJECT]",
		Short: "Check for a new release now instead of in the next poll; --force deploys the current one again",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			project := ""
			if len(args) == 1 {
				project = args[0]
			}
			cs, err := clients(cmd)
			if project != "" {
				var c *client.Client
				c, err = clientFor(cmd, project)
				cs = []*client.Client{c}
			}
			if err != nil {
				return err
			}
			v, failed := newDeployView(os.Stdout), false
			for _, c := range cs {
				v.name, v.shown = c.Name, 0
				body := map[string]any{"project": project, "force": force}
				err := c.Stream(cmd.Context(), "/v1/deploy", body, server.Streamed, func(b []byte) error { return v.event(b, &failed) })
				v.close()
				if err != nil {
					return err
				}
				if v.shown == 0 {
					fmt.Println(v.l.bold(c.Name) + "  " + v.l.dim("nothing new"))
				}
			}
			if failed {
				return ErrShown
			}
			return nil
		},
	}
	c.Flags().BoolVar(&force, "force", false, "deploy even a release that was deployed or failed before")
	return c
}

func rollbackCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rollback PROJECT",
		Short: "Deploy the release before the current one, and hold the current one back until a newer comes",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := clientFor(cmd, args[0])
			if err != nil {
				return err
			}
			v, failed := newDeployView(os.Stdout), false
			v.name = cl.Name
			err = cl.Stream(cmd.Context(), "/v1/rollback", map[string]string{"project": args[0]}, server.Streamed, func(b []byte) error { return v.event(b, &failed) })
			v.close()
			if err != nil {
				return err
			}
			if failed {
				return ErrShown
			}
			return nil
		},
	}
}

func backupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "backup",
		Short: "Back up every database and volume now, as the daily backups do",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cs, err := clients(cmd)
			if err != nil {
				return err
			}
			var errs []error
			for _, c := range cs {
				var run server.BackupRun
				if err := c.Post(cmd.Context(), "/v1/backup", struct{}{}, &run); err != nil {
					errs = append(errs, err)
					continue
				}
				printBackupRun(os.Stdout, c.Name, run)
				if len(run.Errors) > 0 {
					errs = append(errs, fmt.Errorf("%s: %d of %d backups failed", c.Name, len(run.Errors), len(run.Made)+len(run.Errors)))
				}
			}
			return errors.Join(errs...)
		},
	}
}

func backupsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "backups PROJECT",
		Short: "List a project's backups, newest first",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := clientFor(cmd, args[0])
			if err != nil {
				return err
			}
			var ms []backups.Manifest
			if err := cl.Get(cmd.Context(), "/v1/backups?project="+url.QueryEscape(args[0]), &ms); err != nil {
				return err
			}
			printBackups(os.Stdout, cl.Name, args[0], ms)
			return nil
		},
	}
}

// adminClient is the project's server with the admin token, from
// HAKOBU_ADMIN_TOKEN or asked for.
func adminClient(cmd *cobra.Command, project string) (*client.Client, error) {
	cl, err := clientFor(cmd, project)
	if err != nil {
		return nil, err
	}
	// Root on the server is the admin already.
	if cl.Local() && os.Geteuid() == 0 {
		return cl, nil
	}
	tok := os.Getenv("HAKOBU_ADMIN_TOKEN")
	if tok == "" {
		if tok, err = readSecret("admin token of " + cl.Name + ": "); err != nil {
			return nil, err
		}
	}
	return cl.WithToken(tok), nil
}

func restoreCmd() *cobra.Command {
	at := ""
	c := &cobra.Command{
		Use:   "restore PROJECT/NAME",
		Short: "Replace a database or volume (APP/VOLUME) with a backup: the newest, or --at's (needs the admin token)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			project, name, ok := strings.Cut(args[0], "/")
			if !ok {
				return errors.New("name it as PROJECT/DATABASE or PROJECT/APP/VOLUME")
			}
			req := map[string]any{"project": project, "name": name}
			if at != "" {
				t, err := time.Parse(time.RFC3339, at)
				if err != nil {
					return fmt.Errorf("--at: %w (copy it from hakobu backups)", err)
				}
				req["at"] = t
			}
			if err := confirm(fmt.Sprintf("This replaces what %s holds now. Type %s to go on: ", args[0], args[0]), args[0]); err != nil {
				return err
			}
			cl, err := adminClient(cmd, project)
			if err != nil {
				return err
			}
			var out map[string]string
			err = cl.Post(cmd.Context(), "/v1/restore", req, &out)
			fmt.Print(out["log"])
			return err
		},
	}
	c.Flags().StringVar(&at, "at", "", "the backup's time, as hakobu backups shows it")
	return c
}

func destroyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "destroy PROJECT/NAME",
		Short: "Delete a database, cache or volume hakobu.toml no longer has, after a last backup (needs the admin token)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			project, name, ok := strings.Cut(args[0], "/")
			if !ok {
				return errors.New("name it as PROJECT/NAME")
			}
			if err := confirm(fmt.Sprintf("This deletes %s for good (a last backup is kept). Type %s to go on: ", args[0], args[0]), args[0]); err != nil {
				return err
			}
			cl, err := adminClient(cmd, project)
			if err != nil {
				return err
			}
			var out map[string]string
			err = cl.Post(cmd.Context(), "/v1/destroy", map[string]string{"project": project, "name": name, "confirm": args[0]}, &out)
			fmt.Print(out["log"])
			return err
		},
	}
}

// On the project's files.

func secretCmd() *cobra.Command {
	c := &cobra.Command{Use: "secret", Short: "Edit hakobu.secrets.toml: secrets anyone may add and only the server and its owner may read"}
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Start hakobu.secrets.toml, sealed to the server's key and its owner's",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := os.Stat(seal.FileName); err == nil {
				return fmt.Errorf("%s is there already", seal.FileName)
			}
			b, err := os.ReadFile(spec.File)
			if err != nil {
				return err
			}
			p, err := spec.Parse(b)
			if err != nil {
				return err
			}
			if s, _ := cmd.Flags().GetString("server"); s == "" {
				_ = cmd.Flags().Set("server", p.Server)
			}
			cs, err := clients(cmd)
			if err != nil {
				return err
			}
			var key map[string]string
			if err := cs[0].Get(cmd.Context(), "/v1/key", &key); err != nil {
				return err
			}
			s, err := seal.New(key["recipient"], key["owner"])
			if err != nil {
				return err
			}
			return writeSecrets(s)
		},
	}
	set := &cobra.Command{
		Use:   "set APP NAME",
		Short: "Seal a secret (read from stdin, never an argument, so it stays out of shell history); APP hakobu sets hakobu's own",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := readSecrets()
			if err != nil {
				return err
			}
			// A secret is sealed for its project, which hakobu.toml names.
			b, err := os.ReadFile(spec.File)
			if err != nil {
				return err
			}
			p, err := spec.Parse(b)
			if err != nil {
				return fmt.Errorf("%s: %w", spec.File, err)
			}
			v, err := readSecret(args[1] + ": ")
			if err != nil {
				return err
			}
			if v == "" {
				return errors.New("empty value; hakobu secret unset removes one")
			}
			if err := s.Set(p.Name, args[0], args[1], v); err != nil {
				return err
			}
			return writeSecrets(s)
		},
	}
	unset := &cobra.Command{
		Use:   "unset APP NAME",
		Short: "Remove a secret",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := readSecrets()
			if err != nil {
				return err
			}
			if !s.Unset(args[0], args[1]) {
				return fmt.Errorf("%s has no secret %s", args[0], args[1])
			}
			return writeSecrets(s)
		},
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List the secrets' names (their values can't be read here)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := readSecrets()
			if err != nil {
				return err
			}
			var lines []string
			for app, m := range s.Apps {
				for k := range m {
					lines = append(lines, app+"\t"+k)
				}
			}
			for k := range s.Hakobu {
				lines = append(lines, "hakobu\t"+k)
			}
			sort.Strings(lines)
			fmt.Println(strings.Join(lines, "\n"))
			return nil
		},
	}
	c.AddCommand(initCmd, set, unset, list)
	return c
}

func readSecrets() (*seal.Secrets, error) {
	b, err := os.ReadFile(seal.FileName)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no %s here: hakobu secret init", seal.FileName)
	}
	if err != nil {
		return nil, err
	}
	return seal.Parse(b)
}

func writeSecrets(s *seal.Secrets) error {
	b, err := s.Marshal()
	if err != nil {
		return err
	}
	p, err := scaffold.InRepo(".", seal.FileName)
	if err != nil {
		return err
	}
	return scaffold.WriteFile(p, b)
}

func validateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate [FILE]",
		Short: "Check a hakobu.toml (and the hakobu.secrets.toml next to it), as CI should before a release",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			file := spec.File
			if len(args) == 1 {
				file = args[0]
			}
			b, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			p, err := spec.Parse(b)
			if err != nil {
				return fmt.Errorf("%s: %w", file, err)
			}
			if b, err := os.ReadFile(filepath.Join(filepath.Dir(file), seal.FileName)); err == nil {
				s, err := seal.Parse(b)
				if err == nil {
					err = s.CheckApps(slices.Sorted(maps.Keys(p.Apps)))
				}
				if err != nil {
					return fmt.Errorf("%s: %w", seal.FileName, err)
				}
			}
			fmt.Println("ok")
			return nil
		},
	}
}

// Reading from the person at the terminal.

// readSecret reads a line without echoing it from a terminal, or all of
// stdin from a pipe.
func readSecret(prompt string) (string, error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(os.Stderr, prompt)
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		return strings.TrimSpace(string(b)), err
	}
	b, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	return strings.TrimRight(string(b), "\r\n"), err
}

// confirm asks for want to be typed at a terminal; with no terminal (an
// agent, a script) it refuses, so these are a person's to do.
func confirm(prompt, want string) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("this asks a person at a terminal to confirm, and there's none")
	}
	fmt.Fprint(os.Stderr, prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if strings.TrimSpace(line) != want {
		return errors.New("not confirmed; nothing changed")
	}
	return nil
}

func short(image string) string {
	if i := strings.Index(image, "@sha256:"); i >= 0 && len(image) >= i+8+12 {
		return image[:i] + "@" + image[i+8:i+8+12]
	}
	return image
}

func shortRev(rev string) string {
	if len(rev) > 7 {
		return rev[:7]
	}
	return rev
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func ago(t time.Time) string {
	d := time.Since(t).Round(time.Minute)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return t.Local().Format("2006-01-02 15:04")
}
