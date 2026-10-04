package cmd

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/x0ryz/hakobu/internal/ops"
)

// Databases from the command line, on the panel's server. A dump moving in
// is too big for the panel's page (Cloudflare caps a request at 100 MB):
//   ssh old-host pg_dump -Fc mydb | sudo -u hakobu ./hakobu database import mydb -

var databaseCmd = &cobra.Command{
	Use:     "database",
	Aliases: []string{"db"},
	Short:   "The databases of this panel's projects",
}

var databaseImportCmd = &cobra.Command{
	Use:   "import <database> <dump file, or - for stdin>",
	Short: "Add a pg_dump -Fc made elsewhere to a database's backups, to restore from the panel",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		var dump io.Reader = os.Stdin
		if args[1] != "-" {
			f, err := os.Open(args[1])
			if err != nil {
				return err
			}
			defer f.Close()
			dump = f
		}
		s, err := openPanelStore()
		if err != nil {
			return err
		}
		if _, err := ops.ImportDump(s, args[0], dump); err != nil {
			return err
		}
		fmt.Printf("Imported. In the panel, open database %s: Check tries the dump in a scratch database, Restore replaces the database's data with it.\n", args[0])
		return nil
	},
}

func init() {
	databaseCmd.AddCommand(databaseImportCmd)
	rootCmd.AddCommand(databaseCmd)
}
