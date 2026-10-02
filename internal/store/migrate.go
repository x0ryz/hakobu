package store

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

//go:embed telemetry/migrations/*.sql
var telemetryMigrationFiles embed.FS

// errNewerSchema: the database was migrated by a newer hakobu.
var errNewerSchema = errors.New("upgrade hakobu")

// migrate applies the panel's migrations.
func migrate(db *sql.DB) error {
	return migrateWith(db, migrationFiles, "migrations/*.sql")
}

// migrateWith applies the files matching pattern in file-name order. PRAGMA
// user_version holds how many have been applied; each one runs in its own
// transaction together with the version bump. Never edit an applied
// migration, add a new file instead.
func migrateWith(db *sql.DB, files fs.FS, pattern string) error {
	names, err := fs.Glob(files, pattern)
	if err != nil {
		return err
	}
	sort.Strings(names)

	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > len(names) {
		return fmt.Errorf("database is at schema version %d, this hakobu only knows %d: %w", version, len(names), errNewerSchema)
	}

	for i := version; i < len(names); i++ {
		body, err := fs.ReadFile(files, names[i])
		if err != nil {
			return err
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("%s: %w", names[i], err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// SchemaVersion is the schema version this hakobu migrates databases to.
func SchemaVersion() int {
	names, _ := fs.Glob(migrationFiles, "migrations/*.sql")
	return len(names)
}

// DatabaseVersion reads the schema version of the database at path,
// without migrating it or needing the master key.
func DatabaseVersion(path string) (int, error) {
	if _, err := os.Stat(path); err != nil {
		return 0, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var version int
	err = db.QueryRow(`PRAGMA user_version`).Scan(&version)
	return version, err
}

// CopyDatabase writes a consistent copy of the database at path to dst,
// which must not exist, without migrating it or needing the master key.
func CopyDatabase(path, dst string) error {
	if _, err := os.Stat(path); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`VACUUM INTO ?`, dst)
	return err
}
