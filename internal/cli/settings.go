package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/hakobudev/hakobu/internal/alert"
	"github.com/hakobudev/hakobu/internal/client"
	"github.com/hakobudev/hakobu/internal/server"
)

// A server's settings from the laptop: what used to be edited in its
// server.toml (releases, registries, buckets, mail, updates) goes
// through its API, which checks a change, keeps it and restarts hakobu
// for it. Passwords and keys come from the environment or are typed,
// never in arguments.

// settingsWait is how long a change may take to be in effect: hakobu
// restarts for it once no deploy or backup runs.
var settingsWait = 30 * time.Minute

var settingsPoll = 2 * time.Second

func serverGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get [NAME]",
		Short: "Show a server's settings: releases, registries, backups, mail, updates (never a password or key)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			args, err := nameFirst(args, 1, 1)
			if err != nil {
				return err
			}
			cl, err := namedClient(args[0])
			if err != nil {
				return err
			}
			var v server.SettingsView
			if err := cl.Get(cmd.Context(), "/v1/settings", &v); err != nil {
				return err
			}
			printSettings(cmd.OutOrStdout(), v)
			return nil
		},
	}
}

func printSettings(w io.Writer, v server.SettingsView) {
	from := func(src string) string {
		if src == "server.toml" {
			return "  (server.toml)"
		}
		return ""
	}
	fmt.Fprintln(w, "releases:")
	for _, r := range v.Releases {
		fmt.Fprintf(w, "  %s%s\n", r.Image, from(r.Source))
	}
	for _, r := range v.Registries {
		fmt.Fprintf(w, "registry: %s as %s%s\n", r.Host, r.Username, from(r.Source))
	}
	if b := v.Backups; b != nil {
		fmt.Fprintf(w, "backups: at %s UTC, %d daily, %d weekly%s\n", b.At, b.Daily, b.Weekly, from(b.Source))
		if p := b.Primary; p != nil {
			fmt.Fprintf(w, "  primary: %s/%s at %s (key %s)\n", p.Bucket, p.Prefix, p.Endpoint, p.AccessKey)
		}
		for _, c := range b.Copies {
			fmt.Fprintf(w, "  copy %s: %s/%s at %s (key %s)\n", c.Name, c.Bucket, c.Prefix, c.Endpoint, c.AccessKey)
		}
	} else {
		fmt.Fprintln(w, "backups: none (server set backups, root's)")
	}
	if m := v.SMTP; m != nil {
		fmt.Fprintf(w, "mail: %s → %s through %s:%d%s\n", m.From, m.To, m.Host, m.Port, from(m.Source))
	} else {
		fmt.Fprintln(w, "mail: none (server set mail)")
	}
	if v.Pulse != "" {
		fmt.Fprintf(w, "pulse: %s\n", v.Pulse)
	}
	fmt.Fprintf(w, "updates: %s at %s UTC%s\n", v.Updates.Auto, v.Updates.At, from(v.Updates.Source))
}

func serverSetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "set",
		Short: "Change a server's settings through its API: releases, registries, backups, mail, the pulse, updates",
	}

	var remove bool
	release := &cobra.Command{
		Use:   "release [NAME] IMAGE:TAG",
		Short: "Deploy a project's release image on the server (hakobu init does), or --remove it",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			args, err := nameFirst(args, 2, 2)
			if err != nil {
				return err
			}
			ch := server.SettingsChange{AddRelease: args[1]}
			if remove {
				ch = server.SettingsChange{RemoveRelease: args[1]}
			}
			return setSettings(cmd.Context(), args[0], "", ch, cmd.OutOrStdout())
		},
	}
	release.Flags().BoolVar(&remove, "remove", false, "stop deploying it (what it deployed stays)")

	var regUser string
	var regRemove bool
	registry := &cobra.Command{
		Use:   "registry [NAME] HOST --username USER",
		Short: "Credentials to pull from a registry (the password from HAKOBU_REGISTRY_PASSWORD, or typed); ghcr.io needs none",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			args, err := nameFirst(args, 2, 2)
			if err != nil {
				return err
			}
			rg := &server.RegistrySet{Host: args[1], Username: regUser, Remove: regRemove}
			if !regRemove {
				if regUser == "" {
					return errors.New("--username")
				}
				var err error
				if rg.Password, err = secretFromEnv("HAKOBU_REGISTRY_PASSWORD", "password for "+args[1]+": "); err != nil {
					return err
				}
			}
			return setSettings(cmd.Context(), args[0], "", server.SettingsChange{Registry: rg}, cmd.OutOrStdout())
		},
	}
	registry.Flags().StringVar(&regUser, "username", "", "the registry's user")
	registry.Flags().BoolVar(&regRemove, "remove", false, "forget them")

	var m alert.SMTP
	var mailRemove bool
	mail := &cobra.Command{
		Use:   "mail [NAME] --host HOST --port 465 --from FROM --to TO [--username USER]",
		Short: "Where the server mails you its problems: any SMTP server (the password from HAKOBU_SMTP_PASSWORD, or typed); it sends a mail first to prove it works",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			args, err := nameFirst(args, 1, 1)
			if err != nil {
				return err
			}
			if mailRemove {
				return setSettings(cmd.Context(), args[0], "", server.SettingsChange{RemoveSMTP: true}, cmd.OutOrStdout())
			}
			if m.Username != "" {
				var err error
				if m.Password, err = secretFromEnv("HAKOBU_SMTP_PASSWORD", "password for "+m.Username+": "); err != nil {
					return err
				}
			}
			mm := m
			return setSettings(cmd.Context(), args[0], "", server.SettingsChange{SMTP: &mm}, cmd.OutOrStdout())
		},
	}
	mail.Flags().StringVar(&m.Host, "host", "", "the SMTP server")
	mail.Flags().IntVar(&m.Port, "port", 465, "its port: 465 (TLS) or 587 (STARTTLS)")
	mail.Flags().StringVar(&m.Username, "username", "", "the user to sign in as")
	mail.Flags().StringVar(&m.From, "from", "", "the sender")
	mail.Flags().StringVar(&m.To, "to", "", "you")
	mail.Flags().BoolVar(&mailRemove, "remove", false, "mail no more: problems show only in hakobu status")

	pulse := &cobra.Command{
		Use:   "pulse [NAME] URL [FAIL_URL]",
		Short: "A dead man's switch the server fetches every 15 minutes (FAIL_URL while a problem is open), which mails you when it stops",
		Args:  cobra.RangeArgs(1, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			args, err := nameFirst(args, 2, 3)
			if err != nil {
				return err
			}
			p := &server.PulseSet{URL: args[1]}
			if len(args) == 3 {
				p.FailURL = args[2]
			}
			return setSettings(cmd.Context(), args[0], "", server.SettingsChange{Pulse: p}, cmd.OutOrStdout())
		},
	}

	var u server.Updates
	updates := &cobra.Command{
		Use:   "updates [NAME] --auto patch|minor|none [--at 04:00]",
		Short: "Which newer releases of hakobu the server installs by itself, and when (UTC)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			args, err := nameFirst(args, 1, 1)
			if err != nil {
				return err
			}
			uu := u
			return setSettings(cmd.Context(), args[0], "", server.SettingsChange{Updates: &uu}, cmd.OutOrStdout())
		},
	}
	updates.Flags().StringVar(&u.Auto, "auto", "patch", "patch (1.2.x of the 1.2 running), minor (any 1.x) or none")
	updates.Flags().StringVar(&u.At, "at", "", "the time of day, UTC (default an hour after the backups)")

	var bk server.Bucket
	var bs server.BackupsSet
	var copyName, removeCopy string
	var bRemove bool
	backupsCmd := &cobra.Command{
		Use:   "backups [NAME] --endpoint URL --bucket BUCKET --access-key KEY [--copy NAME]",
		Short: "The bucket the server backs up to, or a copy (--copy NAME, a key that may only write): the admin token's (HAKOBU_ADMIN_TOKEN, or typed); the secret key from HAKOBU_S3_SECRET_KEY, or typed",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			args, err := nameFirst(args, 1, 1)
			if err != nil {
				return err
			}
			ch := server.SettingsChange{RemoveBackups: bRemove, RemoveCopyName: removeCopy}
			if !bRemove && removeCopy == "" {
				set := bs
				if bk.Endpoint != "" || bk.Bucket != "" {
					b := bk
					if b.Endpoint == "" || b.Bucket == "" || b.AccessKey == "" {
						return errors.New("a bucket needs --endpoint, --bucket and --access-key")
					}
					var err error
					if b.SecretKey, err = secretFromEnv("HAKOBU_S3_SECRET_KEY", "secret key of "+b.AccessKey+": "); err != nil {
						return err
					}
					if copyName != "" {
						b.Name, set.Copy = copyName, true
					}
					set.Bucket = &b
				}
				ch.Backups = &set
			}
			admin := os.Getenv("HAKOBU_ADMIN_TOKEN")
			// Root on the server is the admin already.
			if cl, err := namedClient(args[0]); err == nil && cl.Local() && os.Geteuid() == 0 {
				return setSettings(cmd.Context(), args[0], "", ch, cmd.OutOrStdout())
			}
			if admin == "" {
				var err error
				if admin, err = readSecret("admin token of " + args[0] + ": "); err != nil {
					return err
				}
			}
			return setSettings(cmd.Context(), args[0], admin, ch, cmd.OutOrStdout())
		},
	}
	f := backupsCmd.Flags()
	f.StringVar(&bk.Endpoint, "endpoint", "", "the S3 endpoint, https://…")
	f.StringVar(&bk.Region, "region", "auto", "its region")
	f.StringVar(&bk.Bucket, "bucket", "", "the bucket")
	f.StringVar(&bk.AccessKey, "access-key", "", "the access key's ID")
	f.StringVar(&bk.Prefix, "prefix", "", "where in the bucket (default hakobu/NAME/)")
	f.StringVar(&copyName, "copy", "", "set a copy of this name instead of the primary")
	f.StringVar(&bs.At, "at", "", "the time of day the backups start, UTC")
	f.IntVar(&bs.Daily, "keep-daily", 0, "daily backups to keep")
	f.IntVar(&bs.Weekly, "keep-weekly", 0, "weekly backups to keep")
	f.BoolVar(&bRemove, "remove", false, "back up no more (the backups made stay in the bucket)")
	f.StringVar(&removeCopy, "remove-copy", "", "drop the copy of this name")

	c.AddCommand(release, registry, mail, pulse, updates, backupsCmd)
	return c
}

// nameFirst puts the server's name first in args when they don't start
// with one and there's one server: on a server (ssh … server set mail …),
// its own.
// The arguments, the name included, are from lo to hi.
func nameFirst(args []string, lo, hi int) ([]string, error) {
	all, err := known()
	if err != nil {
		return nil, err
	}
	if _, ok := all[firstOf(args)]; !ok {
		if len(all) != 1 {
			return nil, errors.New("which server? give its name first")
		}
		args = append([]string{all.Names()[0]}, args...)
	}
	if len(args) < lo || len(args) > hi {
		return nil, fmt.Errorf("%d arguments, the server's name included, where it takes %d to %d", len(args), lo, hi)
	}
	return args, nil
}

func firstOf(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func namedClient(name string) (*client.Client, error) {
	all, err := known()
	if err != nil {
		return nil, err
	}
	return all.Client(name)
}

// postSettings sends a change; it's in effect once hakobu has restarted,
// which it does by itself.
func postSettings(ctx context.Context, name, admin string, ch server.SettingsChange) (*client.Client, int, error) {
	cl, err := namedClient(name)
	if err != nil {
		return nil, 0, err
	}
	if admin != "" {
		cl = cl.WithToken(admin)
	}
	var res struct {
		Rev int `json:"rev"`
	}
	err = cl.Post(ctx, "/v1/settings", ch, &res)
	return cl, res.Rev, err
}

// setSettings sends a change, then waits for hakobu to restart into it.
func setSettings(ctx context.Context, name, admin string, ch server.SettingsChange, out io.Writer) error {
	cl, rev, err := postSettings(ctx, name, admin, ch)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s: saved; hakobu restarts for it once no deploy or backup runs…\n", name)
	deadline := time.Now().Add(settingsWait)
	for {
		var v server.SettingsView
		if err := cl.Get(ctx, "/v1/settings", &v); err == nil && v.Rev >= rev {
			fmt.Fprintf(out, "%s: in effect\n", name)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s: the change is saved, but hakobu hasn't restarted into it yet (a long deploy or backup?): hakobu server get %s shows when", name, name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(settingsPoll):
		}
	}
}

// askSetup asks, at the end of server create, for the backups' bucket and
// the mail server; each may be skipped, for hakobu server set later.
func askSetup(ctx context.Context, name, admin string, out io.Writer) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintf(out, "\nBackups and mail: hakobu server set backups %s …, hakobu server set mail %s …\n", name, name)
		return
	}
	in := bufio.NewReader(os.Stdin)
	ask := func(prompt, def string) string {
		if def != "" {
			prompt += " [" + def + "]"
		}
		fmt.Fprint(os.Stderr, prompt+": ")
		line, _ := in.ReadString('\n')
		if v := strings.TrimSpace(line); v != "" {
			return v
		}
		return def
	}
	fmt.Fprintln(out, "\nBackups go to an S3 bucket (Cloudflare R2, Hetzner, AWS…), sealed. Enter to skip.")
	if endpoint := ask("  endpoint (https://…)", ""); endpoint != "" {
		b := server.Bucket{Endpoint: endpoint, Region: ask("  region", "auto"), Bucket: ask("  bucket", ""), AccessKey: ask("  access key ID", "")}
		var err error
		if b.SecretKey, err = readSecret("  secret key: "); err == nil {
			err = setSettings(ctx, name, admin, server.SettingsChange{Backups: &server.BackupsSet{Bucket: &b}}, out)
		}
		if err != nil {
			fmt.Fprintf(out, "  not set: %v\n  later: hakobu server set backups %s --endpoint … --bucket … --access-key …\n", err, name)
		}
	}
	fmt.Fprintln(out, "\nThe server mails you its problems through any SMTP server. Enter to skip.")
	if host := ask("  SMTP host", ""); host != "" {
		m := alert.SMTP{Host: host, From: ask("  from", ""), To: ask("  to (you)", ""), Username: ask("  username", "")}
		_, _ = fmt.Sscan(ask("  port", "465"), &m.Port)
		var err error
		if m.Username != "" {
			m.Password, err = readSecret("  password: ")
		}
		if err == nil {
			err = setSettings(ctx, name, "", server.SettingsChange{SMTP: &m}, out)
		}
		if err != nil {
			fmt.Fprintf(out, "  not set: %v\n  later: hakobu server set mail %s --host … --from … --to …\n", err, name)
		}
	}
}
