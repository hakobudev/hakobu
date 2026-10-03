package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/ops"
	"github.com/x0ryz/hakobu/internal/store"
)

// Servers from the command line, on the panel's server, as Settings →
// Servers does them: `cd /opt/hakobu && sudo -u hakobu ./hakobu server add`.

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "The other servers this panel runs projects on",
}

var serverAddCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Add a server and print the command that joins it to this panel",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		s, err := openPanelStore()
		if err != nil {
			return err
		}
		token, err := ops.AddServer(s, args[0])
		if err != nil {
			return err
		}
		fmt.Println("Run this on the new server, as root (the token works once, within an hour):")
		fmt.Println()
		fmt.Println("  " + ops.JoinCommand(token))
		return nil
	},
}

var serverListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the servers that joined or are about to",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		s, err := openPanelStore()
		if err != nil {
			return err
		}
		servers, err := ops.Servers(s)
		if err != nil {
			return err
		}
		for _, sv := range servers {
			state := "waiting to join"
			if sv.Joined {
				// Whether it's connected only the running agent knows.
				state = "joined, hakobu " + sv.Version + ", last connected or gone " + sv.LastSeen
			}
			fmt.Printf("%-20s %s\n", sv.Name, state)
		}
		return nil
	},
}

func init() {
	serverCmd.AddCommand(serverAddCmd, serverListCmd)
	rootCmd.AddCommand(serverCmd)
}

// openPanelStore opens the panel's database next to its running agent.
func openPanelStore() (*store.Store, error) {
	if err := config.PrepareDataDir(); err != nil {
		return nil, err
	}
	if config.PublicHost() == "" {
		return nil, fmt.Errorf("this isn't a panel yet: run `hakobu setup` first")
	}
	return store.OpenWithKey(config.DatabaseFile, config.MasterKeyFile)
}
