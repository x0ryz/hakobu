// Package store is hakobu's SQLite state. Queries live in queries.sql and are
// compiled by sqlc (`sqlc generate`) into the *.gen.go files; the schema is
// the migrations directory.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	*Queries
	db *sql.DB
}

func Open(path string) (*Store, error) {
	// WAL + busy_timeout: background deploys and pollers write while the
	// dashboard reads.
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	if err := migrate(db); err != nil {
		return nil, fmt.Errorf("migrate %s (a database from hakobu before 2026-09-25 can't be upgraded, move it away): %w", path, err)
	}
	return &Store{Queries: New(db), db: db}, nil
}

func (a App) ContainerName() string {
	slot := a.ActiveSlot
	if slot == "" {
		slot = "blue"
	}
	return a.Name + "-" + slot
}

func (w Worker) ContainerName() string {
	return w.AppName + "-worker"
}

// DeleteAppCascade removes the app with its worker, volumes, deploy logs and telemetry.
func (s *Store) DeleteAppCascade(ctx context.Context, name string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.WithTx(tx)
	for _, del := range []func(context.Context, string) error{q.DeleteWorker, q.DeleteVolumesOfApp, q.DeleteDeployLogsOfApp, q.DeleteTelemetryOfApp, q.DeleteApp} {
		if err := del(ctx, name); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Owner returns the owner's GitHub login, "" before anyone has claimed the panel.
func (s *Store) Owner(ctx context.Context) (string, error) {
	ac, err := s.GetAccessControl(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return ac.OwnerLogin, err
}

func (s *Store) AllowedLogins(ctx context.Context) ([]string, error) {
	ac, err := s.GetAccessControl(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range strings.Split(ac.AllowedLogins, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *Store) NewSession(ctx context.Context, id, githubLogin string, ttl time.Duration) error {
	return s.CreateSession(ctx, CreateSessionParams{ID: id, GitHubLogin: githubLogin, ExpiresAt: timestamp(time.Now().Add(ttl))})
}

// SessionLogin returns the session's GitHub login; expired sessions are deleted.
func (s *Store) SessionLogin(ctx context.Context, id string) (string, error) {
	sess, err := s.GetSessionRow(ctx, id)
	if err != nil {
		return "", err
	}
	if sess.ExpiresAt < timestamp(time.Now()) {
		s.DeleteSession(ctx, id)
		return "", fmt.Errorf("session expired")
	}
	return sess.GitHubLogin, nil
}

// PruneOldData deletes logs, telemetry and sessions older than retentionDays.
func (s *Store) PruneOldData(ctx context.Context, retentionDays int) error {
	cutoff := timestamp(time.Now().AddDate(0, 0, -retentionDays))
	if err := s.PruneDeployLogs(ctx, cutoff); err != nil {
		return err
	}
	if err := s.PruneTelemetry(ctx, cutoff); err != nil {
		return err
	}
	if err := s.DeleteExpiredSessions(ctx, timestamp(time.Now())); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `VACUUM`)
	return err
}

// timestamp matches the strftime('%Y-%m-%dT%H:%M:%SZ') format of created_at
// columns, so string comparison orders correctly.
func timestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05Z")
}
