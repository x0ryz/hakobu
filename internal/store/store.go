// Package store is hakobu's SQLite state. Queries live in queries.sql and are
// compiled by sqlc (`sqlc generate`) into the *.gen.go files; the schema is
// the migrations directory.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/x0ryz/hakobu/internal/secret"
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
	if err := secret.LoadKey(filepath.Join(filepath.Dir(path), "master.key")); err != nil {
		return nil, fmt.Errorf("master key: %w", err)
	}
	if err := encryptPlaintext(db); err != nil {
		return nil, fmt.Errorf("encrypt secrets: %w", err)
	}
	return &Store{Queries: New(db), db: db}, nil
}

// secretColumns are the columns sqlc maps to secret.String (sqlc.yaml).
var secretColumns = map[string][]string{
	"projects":   {"shared_env"},
	"apps":       {"env", "sentry_key"},
	"workers":    {"env"},
	"databases":  {"db_password"},
	"storages":   {"secret_access_key"},
	"github_app": {"private_key", "webhook_secret", "client_secret"},
	"cloudflare": {"access_token", "refresh_token", "tunnel_token"},
}

// encryptPlaintext encrypts secrets written before hakobu encrypted them.
func encryptPlaintext(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for table, cols := range secretColumns {
		for _, col := range cols {
			rows, err := tx.Query(fmt.Sprintf(`SELECT rowid, %s FROM %s WHERE %s != ''`, col, table, col))
			if err != nil {
				return err
			}
			plain := map[int64]string{}
			for rows.Next() {
				var id int64
				var v string
				if err := rows.Scan(&id, &v); err != nil {
					rows.Close()
					return err
				}
				if !secret.IsEncrypted(v) {
					plain[id] = v
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			for id, v := range plain {
				enc, err := secret.Encrypt(v)
				if err != nil {
					return err
				}
				if _, err := tx.Exec(fmt.Sprintf(`UPDATE %s SET %s = ? WHERE rowid = ?`, table, col), enc, id); err != nil {
					return err
				}
			}
		}
	}
	return tx.Commit()
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

// Sessions are stored by the SHA-256 of their token, so the database
// doesn't hold anything a browser could present.
func sessionID(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Store) NewSession(ctx context.Context, token, githubLogin string, ttl time.Duration) error {
	return s.CreateSession(ctx, CreateSessionParams{ID: sessionID(token), GitHubLogin: githubLogin, ExpiresAt: timestamp(time.Now().Add(ttl))})
}

// SessionLogin returns the session's GitHub login; expired sessions are deleted.
func (s *Store) SessionLogin(ctx context.Context, token string) (string, error) {
	sess, err := s.GetSessionRow(ctx, sessionID(token))
	if err != nil {
		return "", err
	}
	if sess.ExpiresAt < timestamp(time.Now()) {
		s.DeleteSession(ctx, sess.ID)
		return "", fmt.Errorf("session expired")
	}
	return sess.GitHubLogin, nil
}

func (s *Store) EndSession(ctx context.Context, token string) error {
	return s.DeleteSession(ctx, sessionID(token))
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
