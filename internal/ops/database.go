package ops

import (
	"fmt"
	"strings"

	"github.com/x0ryz/hakobu/internal/node"
	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store"
)

// dbSpec is d as its node needs it.
func dbSpec(d store.Database) node.DBSpec {
	return node.DBSpec{Name: d.Name, User: d.User, Password: string(d.Password)}
}

// databaseNode is the node of the database's project.
func databaseNode(s *store.Store, d store.Database) (node.Node, error) {
	p, err := s.GetProjectByID(ctx(), d.ProjectID)
	if err != nil {
		return nil, err
	}
	return ProjectNode(s, p.Name), nil
}

// SuggestDatabaseName is the name the "new database" field offers, and
// takes when left empty (suggestName).
func SuggestDatabaseName(s *store.Store, p store.Project) string {
	return suggestName(p.Name, "db", validDBName, func(name string) bool {
		_, err := s.GetDatabase(ctx(), name)
		return err == nil
	})
}

func CreateDatabase(s *store.Store, projectName, name string) error {
	if !validDBName.MatchString(name) {
		return fmt.Errorf("invalid database name %q: use lowercase letters, digits, dashes and underscores, starting with a letter", name)
	}
	p, err := s.GetProject(ctx(), projectName)
	if err != nil {
		return fmt.Errorf("project %q not found: %w", projectName, err)
	}
	if _, err := s.GetDatabase(ctx(), name); err == nil {
		return fmt.Errorf("a database named %q already exists", name)
	}
	n := ProjectNode(s, projectName)
	if err := n.EnsurePostgres(ctx()); err != nil {
		return fmt.Errorf("failed to start postgres: %w", err)
	}
	if err := ensureProjectNetworks(s, projectName); err != nil {
		return err
	}
	password, err := RandomHex(16)
	if err != nil {
		return err
	}
	d := store.Database{Name: name, User: name + "_user", Password: secret.String(password)}
	if err := n.CreateDatabase(ctx(), dbSpec(d)); err != nil {
		return err
	}
	return s.CreateDatabase(ctx(), store.CreateDatabaseParams{ProjectID: p.ID, Name: d.Name, User: d.User, Password: d.Password})
}

// ensureInPostgres creates d, empty, with its role if Postgres lacks it, as
// on a new server the panel was restored to.
func ensureInPostgres(s *store.Store, d store.Database) error {
	p, err := s.GetProjectByID(ctx(), d.ProjectID)
	if err != nil {
		return err
	}
	n := ProjectNode(s, p.Name)
	if err := n.EnsurePostgres(ctx()); err != nil {
		return fmt.Errorf("failed to start postgres: %w", err)
	}
	if err := ensureProjectNetworks(s, p.Name); err != nil {
		return err
	}
	return n.EnsureDatabase(ctx(), dbSpec(d))
}

func DeleteDatabase(s *store.Store, name string) error {
	d, err := s.GetDatabase(ctx(), name)
	if err != nil {
		return fmt.Errorf("database %q not found: %w", name, err)
	}
	if err := reserveDB(name, "being deleted"); err != nil {
		return err
	}
	defer func() {
		dbJobsMu.Lock()
		delete(dbJobs, name)
		dbJobsMu.Unlock()
	}()
	linked, err := s.AppsUsingDatabase(ctx(), name)
	if err != nil {
		return err
	}
	if len(linked) > 0 {
		return fmt.Errorf("database %s is still used by %s — unlink it first", name, strings.Join(linked, ", "))
	}
	n, err := databaseNode(s, d)
	if err != nil {
		return err
	}
	if err := n.DropDatabase(ctx(), dbSpec(d)); err != nil {
		return err
	}
	// The files stay in the storage; a new database with the same name
	// mustn't list (and rotate away) the old one's backups.
	if err := s.DeleteBackupsOf(ctx(), name); err != nil {
		return err
	}
	return s.DeleteDatabase(ctx(), name)
}

// DatabaseReady reports whether Postgres answers on the database's node.
func DatabaseReady(s *store.Store, d store.Database) bool {
	n, err := databaseNode(s, d)
	return err == nil && n.PostgresReady(ctx())
}

func DatabaseEnv(d store.Database) []string {
	return []string{
		"DATABASE_URL=" + fmt.Sprintf("postgres://%s:%s@%s:5432/%s", d.User, d.Password, node.PostgresContainer, d.Name),
		"POSTGRES_HOST=" + node.PostgresContainer,
		"POSTGRES_PORT=5432",
		"POSTGRES_DB=" + d.Name,
		"POSTGRES_USER=" + d.User,
		"POSTGRES_PASSWORD=" + string(d.Password),
	}
}
