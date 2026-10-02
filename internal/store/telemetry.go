package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// The apps' telemetry lives in telemetry.db next to the panel's database:
// it's the bulk of the data, changes all the time and is worth little a
// month later, so it isn't in the backups or the copy kept for rolling
// hakobu back. Losing it loses only history; hakobu starts a new one
// whenever it can't use the file.
const telemetryFile = "telemetry.db"

// eventRetention is how long errors, crashes and outages are kept; the
// apps' logs go after PruneOldData's retentionDays.
const eventRetention = 30 * 24 * time.Hour

// telSecretColumns are the columns of telemetry.db sqlc maps to
// secret.String (sqlc.yaml): apps' errors and logs may hold their users'
// data.
var telSecretColumns = map[string][]string{
	"telemetry_events": {"message", "payload"},
}

// openTelemetry opens telemetry.db in dir, starting a new one when it was
// written by a newer hakobu or under a master key that's gone.
func openTelemetry(dir string, keyMissing bool) (*sql.DB, error) {
	path := filepath.Join(dir, telemetryFile)
	if keyMissing {
		removeTelemetry(path)
	}
	open := func() (*sql.DB, error) {
		// auto_vacuum only takes effect on a new file: pruning then gives
		// the space back without rewriting the whole file.
		db, err := sql.Open("sqlite", path+"?_pragma=auto_vacuum(INCREMENTAL)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
		if err != nil {
			return nil, err
		}
		if err := migrateWith(db, telemetryMigrationFiles, "telemetry/migrations/*.sql"); err != nil {
			db.Close()
			return nil, err
		}
		return db, nil
	}
	db, err := open()
	if errors.Is(err, errNewerSchema) {
		fmt.Println(telemetryFile, "is from a newer hakobu; starting a new one")
		removeTelemetry(path)
		db, err = open()
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return db, nil
}

func removeTelemetry(path string) {
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Println("failed to remove", f+":", err)
		}
	}
}

// moveTelemetry copies the last month of telemetry from a panel database
// that still has it (migration 008 then drops it) into an empty
// telemetry.db; has reports that the panel's database had it. A hakobu
// rolled back and upgraded again finds telemetry.db filled and drops what
// the older one recorded in between.
func moveTelemetry(db, telDB *sql.DB, telPath string) (has bool, err error) {
	if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'telemetry_events')`).Scan(&has); err != nil || !has {
		return false, err
	}
	var filled bool
	if err := telDB.QueryRow(`SELECT EXISTS (SELECT 1 FROM telemetry_events)`).Scan(&filled); err != nil || filled {
		return true, err
	}
	ctx := context.Background()
	conn, err := db.Conn(ctx) // ATTACH holds for one connection only
	if err != nil {
		return true, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS tel`, telPath); err != nil {
		return true, err
	}
	// conn goes back to the pool: leave it as it was.
	defer func() { _, _ = conn.ExecContext(ctx, `DETACH DATABASE tel`) }()
	_, err = conn.ExecContext(ctx, `INSERT INTO tel.telemetry_events (id, app_name, kind, level, message, payload, created_at)
		SELECT id, app_name, kind, level, message, payload, created_at FROM telemetry_events WHERE created_at >= ?`,
		timestamp(time.Now().Add(-eventRetention)))
	return true, err
}

// reencryptTelemetry encrypts the telemetry with the current master key.
func (s *Store) reencryptTelemetry() error {
	tx, err := s.telDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := reencrypt(tx, telSecretColumns); err != nil {
		return err
	}
	return tx.Commit()
}

// dropTelemetry empties the tables holding encrypted telemetry.
func (s *Store) dropTelemetry() {
	for table := range telSecretColumns {
		if _, err := s.telDB.Exec(`DELETE FROM ` + table); err != nil {
			fmt.Println("failed to empty", table+":", err)
		}
	}
}

// deleteTelemetryOf removes the telemetry of a deleted app.
func (s *Store) deleteTelemetryOf(ctx context.Context, app string) error {
	return s.Tel.DeleteTelemetryOfApp(ctx, app)
}

// pruneTelemetry deletes logs older than retentionDays and other telemetry
// older than eventRetention, and gives the space back.
func (s *Store) pruneTelemetry(ctx context.Context, retentionDays int) error {
	if err := s.Tel.PruneLogs(ctx, timestamp(time.Now().AddDate(0, 0, -retentionDays))); err != nil {
		return err
	}
	if err := s.Tel.PruneEvents(ctx, timestamp(time.Now().Add(-eventRetention))); err != nil {
		return err
	}
	_, err := s.telDB.ExecContext(ctx, `PRAGMA incremental_vacuum`)
	return err
}
