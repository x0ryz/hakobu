package ops

import (
	"fmt"
	"strings"
	"time"

	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/deploy"
	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store"
)

// PostgresContainer is the one shared Postgres service; every database is a
// logical database + role inside it. Tests run their own.
var PostgresContainer = "hakobu-postgres"

// ensurePostgres starts the shared service on first use; an existing
// container is reused, never recreated. The superuser password only
// satisfies the image's startup check: hakobu administers the service over
// `docker exec` with local trust auth.
func ensurePostgres() error {
	existing, err := deploy.ContainerEnv(ctx(), PostgresContainer)
	if err != nil {
		return err
	}
	if existing != nil {
		err = deploy.StartContainer(ctx(), PostgresContainer)
	} else {
		var password string
		if password, err = RandomHex(16); err != nil {
			return err
		}
		// Postgres 18+ images expect the volume at /var/lib/postgresql, older ones work with it too.
		_, err = deploy.RunServiceContainer(ctx(), PostgresContainer, config.PostgresImage, []string{"POSTGRES_PASSWORD=" + password}, "/var/lib/postgresql")
	}
	if err != nil {
		return err
	}
	if !deploy.WaitHealthy(60, time.Second, func() bool { return deploy.PostgresReady(ctx(), PostgresContainer) }) {
		return fmt.Errorf("postgres service failed to become ready")
	}
	return nil
}

// SuggestDatabaseName is the name the "new database" field offers, and
// takes when left empty, so a name doesn't have to be invented: the project
// name if it's a free valid database name, "main" for the project's first
// database, "" (type your own) otherwise. It's a placeholder, not a value,
// so typing doesn't add to it.
func SuggestDatabaseName(s *store.Store, p store.Project) string {
	clean := strings.ToLower(p.Name)
	clean = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, clean)
	clean = strings.Trim(clean, "-_")
	for _, cand := range []string{clean, "main"} {
		if !validDBName.MatchString(cand) {
			continue
		}
		if _, err := s.GetDatabase(ctx(), cand); err != nil {
			return cand
		}
	}
	return ""
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
	if err := ensurePostgres(); err != nil {
		return fmt.Errorf("failed to start postgres: %w", err)
	}
	if err := ensureProjectNetworks(projectName); err != nil {
		return err
	}
	password, err := RandomHex(16)
	if err != nil {
		return err
	}
	d := store.Database{Name: name, User: name + "_user", Password: secret.String(password)}
	if err := createInPostgres(d); err != nil {
		return err
	}
	return s.CreateDatabase(ctx(), store.CreateDatabaseParams{ProjectID: p.ID, Name: d.Name, User: d.User, Password: d.Password})
}

func createInPostgres(d store.Database) error {
	for _, sql := range []string{
		fmt.Sprintf(`CREATE USER "%s" WITH PASSWORD '%s'`, d.User, string(d.Password)),
		fmt.Sprintf(`CREATE DATABASE "%s" OWNER "%s"`, d.Name, d.User),
		// New databases are connectable by PUBLIC; keep other projects' roles out.
		fmt.Sprintf(`REVOKE CONNECT ON DATABASE "%s" FROM PUBLIC`, d.Name),
	} {
		if err := deploy.PostgresExec(ctx(), PostgresContainer, sql); err != nil {
			return err
		}
	}
	return nil
}

// ensureInPostgres creates d, empty, with its role if Postgres lacks it, as
// on a new server the panel was restored to.
func ensureInPostgres(s *store.Store, d store.Database) error {
	if err := ensurePostgres(); err != nil {
		return fmt.Errorf("failed to start postgres: %w", err)
	}
	if p, err := s.GetProjectByID(ctx(), d.ProjectID); err == nil {
		if err := ensureProjectNetworks(p.Name); err != nil {
			return err
		}
	}
	exists, err := deploy.PostgresHasDatabase(ctx(), PostgresContainer, d.Name)
	if err != nil || exists {
		return err
	}
	return createInPostgres(d)
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
	for _, sql := range []string{
		fmt.Sprintf(`DROP DATABASE IF EXISTS "%s" WITH (FORCE)`, d.Name),
		fmt.Sprintf(`DROP USER IF EXISTS "%s"`, d.User),
	} {
		if err := deploy.PostgresExec(ctx(), PostgresContainer, sql); err != nil {
			return err
		}
	}
	// The files stay in the storage; a new database with the same name
	// mustn't list (and rotate away) the old one's backups.
	if err := s.DeleteBackupsOf(ctx(), name); err != nil {
		return err
	}
	return s.DeleteDatabase(ctx(), name)
}

func DatabaseReady() bool {
	return deploy.PostgresReady(ctx(), PostgresContainer)
}

func DatabaseEnv(d store.Database) []string {
	return []string{
		"DATABASE_URL=" + fmt.Sprintf("postgres://%s:%s@%s:5432/%s", d.User, d.Password, PostgresContainer, d.Name),
		"POSTGRES_HOST=" + PostgresContainer,
		"POSTGRES_PORT=5432",
		"POSTGRES_DB=" + d.Name,
		"POSTGRES_USER=" + d.User,
		"POSTGRES_PASSWORD=" + string(d.Password),
	}
}
