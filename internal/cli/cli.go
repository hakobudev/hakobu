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
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/hakobudev/hakobu/internal/apply"
	"github.com/hakobudev/hakobu/internal/backups"
	"github.com/hakobudev/hakobu/internal/client"
	"github.com/hakobudev/hakobu/internal/deploy"
	"github.com/hakobudev/hakobu/internal/node"
	"github.com/hakobudev/hakobu/internal/seal"
	"github.com/hakobudev/hakobu/internal/server"
	"github.com/hakobudev/hakobu/internal/spec"
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
	root.AddCommand(serveCmd(), serverCmd(), statusCmd(), deploysCmd(), logsCmd(), deployCmd(), rollbackCmd(),
		backupCmd(), backupsCmd(), restoreCmd(), destroyCmd(), secretCmd(), validateCmd(), mcpCmd(), dialerCmd())
	return root
}

// On the server.

func serveCmd() *cobra.Command {
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
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return d.Run(ctx)
		},
	}
	c.Flags().StringVar(&config, "config", config, "the server's config")
	return c
}

func serverCmd() *cobra.Command {
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
		Short: "Remember a server: its API's URL on the tailnet and the operator token (read from stdin)",
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
	c.AddCommand(initCmd, token, add, list)
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

// clients are the servers to ask: --server's, or all of them.
func clients(cmd *cobra.Command) ([]*client.Client, error) {
	_, all, err := servers()
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, errors.New("no servers yet: hakobu server add NAME URL")
	}
	names := all.Names()
	if s, _ := cmd.Flags().GetString("server"); s != "" {
		names = []string{s}
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

func printStatus(w io.Writer, st server.Status) {
	fmt.Fprintln(w, st.Server)
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	projects := make([]string, 0, len(st.Apps))
	for p := range st.Apps {
		projects = append(projects, p)
	}
	sort.Strings(projects)
	for _, p := range projects {
		apps := make([]string, 0, len(st.Apps[p]))
		for a := range st.Apps[p] {
			apps = append(apps, a)
		}
		sort.Strings(apps)
		for _, a := range apps {
			as := st.Apps[p][a]
			fmt.Fprintf(tw, "  app\t%s/%s\t%s\t127.0.0.1:%d\n", p, a, short(as.Image), as.ProxyPort)
		}
	}
	for _, r := range st.Resources {
		note := ""
		if r.Orphan {
			note = "not in hakobu.toml: hakobu destroy " + r.Project + "/" + r.Name
		}
		fmt.Fprintf(tw, "  %s\t%s/%s\t%s\n", r.Kind, r.Project, r.Name, note)
	}
	tw.Flush()
	if d := st.LastDeploy; d != nil {
		outcome := "ok"
		if d.Error != "" {
			outcome = "FAILED: " + firstLine(d.Error)
		}
		fmt.Fprintf(w, "  last deploy: %s %s %s, %s\n", ago(d.At), d.Project, shortRev(d.Revision), outcome)
	}
	for _, p := range st.Problems {
		fmt.Fprintf(w, "  PROBLEM since %s: %s\n", ago(p.Since), p.Subject)
	}
	if !st.Mail {
		fmt.Fprintln(w, "  mail: none configured — problems show only here")
	}
	switch b := st.LastBackup; {
	case !st.Backups:
		fmt.Fprintln(w, "  backups: NONE — no bucket configured")
	case b == nil:
		fmt.Fprintln(w, "  backups: none since hakobu started")
	case len(b.Errors) > 0:
		fmt.Fprintf(w, "  backups: %s, %d made, %d FAILED: %s\n", ago(b.At), len(b.Made), len(b.Errors), firstLine(b.Errors[0]))
	default:
		fmt.Fprintf(w, "  backups: %s, %d made\n", ago(b.At), len(b.Made))
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
			for _, c := range cs {
				var rs []apply.Result
				if err := c.Get(cmd.Context(), fmt.Sprintf("/v1/deploys?n=%d", n), &rs); err != nil {
					return err
				}
				for _, r := range rs {
					outcome := fmt.Sprintf("%d changes", len(r.Changes))
					if r.Error != "" {
						outcome = "FAILED: " + firstLine(r.Error)
					}
					fmt.Printf("%s  %s  %s %s  %s\n", r.At.Local().Format("2006-01-02 15:04"), c.Name, r.Project, shortRev(r.Revision), outcome)
				}
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
			q := fmt.Sprintf("/v1/logs?project=%s&app=%s&lines=%d", project, app, lines)
			if worker {
				q += "&worker=1"
			}
			if err := cl.Get(cmd.Context(), q, &out); err != nil {
				return err
			}
			fmt.Print(out["logs"])
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
			for _, c := range cs {
				var rs []map[string]any
				if err := c.Post(cmd.Context(), "/v1/deploy", map[string]any{"project": project, "force": force}, &rs); err != nil {
					return err
				}
				if len(rs) == 0 {
					fmt.Printf("%s: nothing new\n", c.Name)
				}
				if err := printResults(rs); err != nil {
					return err
				}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&force, "force", false, "deploy even a release that was deployed or failed before")
	return c
}

func printResults(rs []map[string]any) error {
	var errs []error
	for _, r := range rs {
		fmt.Print(r["log"])
		if e, _ := r["error"].(string); e != "" {
			errs = append(errs, errors.New(e))
		}
	}
	return errors.Join(errs...)
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
			var r map[string]any
			if err := cl.Post(cmd.Context(), "/v1/rollback", map[string]string{"project": args[0]}, &r); err != nil {
				return err
			}
			return printResults([]map[string]any{r})
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
				for _, m := range run.Made {
					fmt.Printf("%s: %s\n", c.Name, m)
				}
				for _, e := range run.Errors {
					errs = append(errs, fmt.Errorf("%s: %s", c.Name, e))
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
			if err := cl.Get(cmd.Context(), "/v1/backups?project="+args[0], &ms); err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
			for _, m := range ms {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", m.At.Format(time.RFC3339), m.Kind, m.Name, m.Checked)
			}
			return tw.Flush()
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
			v, err := readSecret(args[1] + ": ")
			if err != nil {
				return err
			}
			if v == "" {
				return errors.New("empty value; hakobu secret unset removes one")
			}
			if err := s.Set(args[0], args[1], v); err != nil {
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
	return os.WriteFile(seal.FileName, b, 0o644)
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
			if _, err := spec.Parse(b); err != nil {
				return fmt.Errorf("%s: %w", file, err)
			}
			if b, err := os.ReadFile(seal.FileName); err == nil {
				if _, err := seal.Parse(b); err != nil {
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
