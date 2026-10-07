package ops

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/hakobudev/hakobu/internal/node"
	"github.com/hakobudev/hakobu/internal/secret"
	"github.com/hakobudev/hakobu/internal/store"
)

// dbSpec is d as its node needs it: in Postgres it's d.Key(), with a
// role of that name.
func dbSpec(d store.Database) node.DBSpec {
	return node.DBSpec{Name: d.Key(), User: d.Key(), Password: string(d.Password)}
}

// databaseNode is the node of the database's project.
func databaseNode(s *store.Store, d store.Database) (node.Node, error) {
	return ProjectNode(s, d.ProjectID), nil
}

// SuggestDatabaseName is the name the "new database" field offers, and
// takes when left empty (suggestName).
func SuggestDatabaseName(s *store.Store, p store.Project) string {
	return suggestName(p.Name, "db", validDBName, func(name string) bool {
		_, err := s.GetDatabase(ctx(), store.GetDatabaseParams{ProjectID: p.ID, Name: name})
		return err == nil
	})
}

// CreateDatabase creates a database named name in the project.
func CreateDatabase(s *store.Store, projectID int64, name string) (int64, error) {
	if !validDBName.MatchString(name) {
		return 0, fmt.Errorf("invalid database name %q: use lowercase letters, digits, dashes and underscores, starting with a letter", name)
	}
	if _, err := s.GetProjectByID(ctx(), projectID); err != nil {
		return 0, fmt.Errorf("project not found: %w", err)
	}
	n := ProjectNode(s, projectID)
	if err := n.EnsurePostgres(ctx()); err != nil {
		return 0, fmt.Errorf("failed to start postgres: %w", err)
	}
	if err := ensureProjectNetworks(s, projectID); err != nil {
		return 0, err
	}
	password, err := RandomHex(16)
	if err != nil {
		return 0, err
	}
	// The record first: its ID names the database in Postgres.
	id, err := s.CreateDatabase(ctx(), store.CreateDatabaseParams{ProjectID: projectID, Name: name, Password: secret.String(password)})
	if err != nil {
		return 0, taken(err, "a database named %q", name)
	}
	d, err := s.GetDatabaseByID(ctx(), id)
	if err == nil {
		err = n.CreateDatabase(ctx(), dbSpec(d))
	}
	if err != nil {
		_ = s.DeleteDatabase(ctx(), id)
		return 0, err
	}
	return id, nil
}

// ensureInPostgres creates d, empty, with its role if Postgres lacks it, as
// on a new server the panel was restored to.
func ensureInPostgres(s *store.Store, d store.Database) error {
	n := ProjectNode(s, d.ProjectID)
	if err := n.EnsurePostgres(ctx()); err != nil {
		return fmt.Errorf("failed to start postgres: %w", err)
	}
	if err := ensureProjectNetworks(s, d.ProjectID); err != nil {
		return err
	}
	return n.EnsureDatabase(ctx(), dbSpec(d))
}

func DeleteDatabase(s *store.Store, id int64) error {
	d, err := s.GetDatabaseByID(ctx(), id)
	if err != nil {
		return fmt.Errorf("database not found: %w", err)
	}
	if err := reserveDB(dbJobKey(id), "being deleted"); err != nil {
		return err
	}
	defer func() {
		dbJobsMu.Lock()
		delete(dbJobs, dbJobKey(id))
		dbJobsMu.Unlock()
	}()
	linked, err := s.AppsUsingDatabase(ctx(), sql.NullInt64{Int64: id, Valid: true})
	if err != nil {
		return err
	}
	if len(linked) > 0 {
		return fmt.Errorf("database %s is still used by %s — unlink it first", d.Name, strings.Join(linked, ", "))
	}
	n, err := databaseNode(s, d)
	if err != nil {
		return err
	}
	if err := n.DropDatabase(ctx(), dbSpec(d)); err != nil {
		return err
	}
	// The files stay in the bucket, out of the panel's sight.
	if err := s.DeleteBackupsOf(ctx(), id); err != nil {
		return err
	}
	return s.DeleteDatabase(ctx(), id)
}

// DatabaseReady reports whether Postgres answers on the database's node.
func DatabaseReady(s *store.Store, d store.Database) bool {
	n, err := databaseNode(s, d)
	return err == nil && n.PostgresReady(ctx())
}

func DatabaseEnv(d store.Database) []string {
	return []string{
		"DATABASE_URL=" + fmt.Sprintf("postgres://%s:%s@%s:5432/%s", d.Key(), d.Password, node.PostgresContainer, d.Key()),
		"POSTGRES_HOST=" + node.PostgresContainer,
		"POSTGRES_PORT=5432",
		"POSTGRES_DB=" + d.Key(),
		"POSTGRES_USER=" + d.Key(),
		"POSTGRES_PASSWORD=" + string(d.Password),
	}
}
