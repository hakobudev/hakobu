package cmd

import (
	"context"
	"fmt"
	"github.com/x0ryz/hakobu/internal/store"
	"io"
	"os"
	"strings"

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
	Use:   "import <project>/<database> <dump file, or - for stdin>",
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
		d, err := findDatabase(s, args[0], importUser)
		if err != nil {
			return err
		}
		if _, err := ops.ImportDump(s, d.ID, dump); err != nil {
			return err
		}
		fmt.Printf("Imported. In the panel, open database %s: Check tries the dump in a scratch database, Restore replaces the database's data with it.\n", args[0])
		return nil
	},
}

var importUser string

// findDatabase finds project/database, of the user with GitHub login user
// when several users have a project of that name.
func findDatabase(s *store.Store, path, user string) (store.Database, error) {
	project, name, ok := strings.Cut(path, "/")
	if !ok {
		return store.Database{}, fmt.Errorf("name the database as <project>/<database>")
	}
	projects, err := s.ListProjects(context.Background())
	if err != nil {
		return store.Database{}, err
	}
	var found []store.Project
	for _, p := range projects {
		if p.Name != project {
			continue
		}
		if user != "" {
			if u, err := s.GetUser(context.Background(), p.UserID); err != nil || !strings.EqualFold(u.GitHubLogin, user) {
				continue
			}
		}
		found = append(found, p)
	}
	switch {
	case len(found) == 0:
		return store.Database{}, fmt.Errorf("no project %q", project)
	case len(found) > 1:
		return store.Database{}, fmt.Errorf("several users have a project %q: say whose with --user <GitHub login>", project)
	}
	d, err := s.GetDatabase(context.Background(), store.GetDatabaseParams{ProjectID: found[0].ID, Name: name})
	if err != nil {
		return d, fmt.Errorf("no database %q in project %s", name, project)
	}
	return d, nil
}

func init() {
	databaseImportCmd.Flags().StringVar(&importUser, "user", "", "the GitHub login of the project's owner, when several users have a project of that name")
	databaseCmd.AddCommand(databaseImportCmd)
	rootCmd.AddCommand(databaseCmd)
}
