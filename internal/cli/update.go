package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/hakobudev/hakobu/internal/client"
	"github.com/hakobudev/hakobu/internal/server"
	"github.com/hakobudev/hakobu/internal/update"
	"github.com/spf13/cobra"
)

func serverUpdateCmd(version string, config *string) *cobra.Command {
	var requested bool
	var to string
	c := &cobra.Command{
		Use:   "update [NAME]",
		Short: "Update hakobu on a server (every server without NAME) to a newer signed release, the latest or --to's; one that doesn't come up is replaced by the one before",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if to != "" {
				to = update.Tag(to)
			}
			if requested || len(args) == 0 && onServer(*config) {
				return updateHere(version, *config, to, requested)
			}
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			cs, err := pick(cmd, name)
			if err != nil {
				return err
			}
			var errs []error
			for _, c := range cs {
				st, err := updateServer(cmd.Context(), c, to, os.Stderr)
				switch {
				case err != nil:
					fmt.Printf("%s: %v\n", c.Name, err)
					errs = append(errs, err)
				case st.State == "up to date":
					fmt.Printf("%s: hakobu %s is the latest\n", c.Name, st.From)
				default:
					fmt.Printf("%s: hakobu %s → %s\n", c.Name, st.From, st.To)
				}
			}
			return errors.Join(errs...)
		},
	}
	c.Flags().StringVar(config, "config", *config, "the server's config (on the server)")
	c.Flags().StringVar(&to, "to", "", "the release to install (default: the latest)")
	c.Flags().BoolVar(&requested, "requested", false, "on the server: the release hakobu asked for (hakobu-update.service runs it so)")
	_ = c.Flags().MarkHidden("requested")
	return c
}

// onServer tells whether this runs on a server, as root: then an update
// without a server's name is of this one.
func onServer(config string) bool {
	if os.Geteuid() != 0 {
		return false
	}
	_, err := os.Stat(config)
	return err == nil
}

func updateHere(version, config, to string, requested bool) error {
	if os.Geteuid() != 0 {
		return errors.New("run it on the server as root, or name a server")
	}
	cfg, err := server.LoadConfig(config)
	if err != nil {
		return fmt.Errorf("%s: %w", config, err)
	}
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	in := &update.Install{Dir: filepath.Dir(bin), Data: cfg.DataDir, Version: version, Listen: cfg.Listen, Out: os.Stdout}
	if requested {
		return in.UpdateRequested()
	}
	return in.Update(to)
}

// updatePoll is how often updateServer asks how the update goes.
var updatePoll = 3 * time.Second

// updateServer asks a server to update hakobu and waits for how it went:
// the server restarts on the way, so it doesn't answer for a while.
func updateServer(ctx context.Context, c *client.Client, to string, progress io.Writer) (update.Status, error) {
	var before server.Status
	if err := c.Get(ctx, "/v1/status", &before); err != nil {
		return update.Status{}, err
	}
	var asked map[string]string
	if err := c.Post(ctx, "/v1/update", map[string]string{"to": to}, &asked); err != nil {
		return update.Status{}, err
	}
	fmt.Fprintf(progress, "%s: updating hakobu %s to %s…\n", c.Name, asked["from"], asked["to"])
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return update.Status{}, ctx.Err()
		case <-time.After(updatePoll):
		}
		var st server.Status
		if c.Get(ctx, "/v1/status", &st) != nil {
			continue // restarting
		}
		u := st.LastUpdate
		if u == nil || u.State == "running" || before.LastUpdate != nil && !u.At.After(before.LastUpdate.At) {
			continue
		}
		switch u.State {
		case "updated", "up to date":
			return *u, nil
		default:
			return *u, fmt.Errorf("%s: %s", u.State, u.Message)
		}
	}
	return update.Status{}, errors.New("the update didn't finish in 10 minutes: hakobu status says how it's going")
}
