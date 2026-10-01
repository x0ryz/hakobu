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
	"time"

	_ "modernc.org/sqlite"

	"github.com/x0ryz/hakobu/internal/secret"
)

type Store struct {
	*Queries
	db      *sql.DB
	keyPath string
}

func Open(path string) (*Store, error) {
	// WAL + busy_timeout: background deploys and pollers write while the
	// dashboard reads.
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	if err := migrate(db); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	s := &Store{Queries: New(db), db: db, keyPath: filepath.Join(filepath.Dir(path), "master.key")}
	if err := checkKeyNotLost(db, s.keyPath, secret.KeyMissing(s.keyPath)); err != nil {
		return nil, err
	}
	if err := secret.LoadKey(s.keyPath); err != nil {
		return nil, fmt.Errorf("master key: %w", err)
	}
	if secret.Rotating() { // cut short last time
		if err := s.finishRotation(); err != nil {
			return nil, fmt.Errorf("finishing the master key rotation: %w", err)
		}
	}
	return s, nil
}

// checkKeyNotLost refuses a missing master key while the database holds
// secrets: a new key would leave every one of them unreadable.
func checkKeyNotLost(db *sql.DB, keyPath string, missing bool) error {
	if !missing {
		return nil
	}
	for table, cols := range secretColumns {
		for _, col := range cols {
			var any bool
			if err := db.QueryRow(fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s WHERE %s != '')`, table, col)).Scan(&any); err != nil {
				return err
			}
			if any {
				return fmt.Errorf("%s is missing, but the database holds secrets encrypted with it (%s.%s): put the key back or set HAKOBU_MASTER_KEY; hakobu won't start with a new key that can't read them", keyPath, table, col)
			}
		}
	}
	return nil
}

// secretColumns are the columns sqlc maps to secret.String (sqlc.yaml);
// TestSecretColumnsMatchSqlc keeps the two in step.
var secretColumns = map[string][]string{
	"projects":    {"shared_env"},
	"apps":        {"env", "sentry_key"},
	"workers":     {"env", "command"},
	"databases":   {"db_password"},
	"storages":    {"secret_access_key"},
	"github_app":  {"private_key", "webhook_secret", "client_secret"},
	"cloudflare":  {"api_token", "tunnel_token"},
	"sealed_vars": {"value"},
	// Apps' errors, logs and traces (they may hold their users' data) and
	// build output (scripts print secrets now and then).
	"telemetry_events": {"message", "payload"},
	"deploy_logs":      {"output"},
}

// RotateMasterKey encrypts every secret with a new master key, so a copy
// of the old key (and of the database) is worth nothing any more.
func (s *Store) RotateMasterKey() error {
	if err := secret.BeginRotation(s.keyPath); err != nil {
		return err
	}
	return s.finishRotation()
}

// finishRotation re-encrypts every secret with the new key in one
// transaction, then retires the old key.
func (s *Store) finishRotation() error {
	tx, err := s.db.Begin()
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
			values := map[int64]string{}
			for rows.Next() {
				var id int64
				var v string
				if err := rows.Scan(&id, &v); err != nil {
					rows.Close()
					return err
				}
				values[id] = v
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			for id, v := range values {
				plain, err := secret.Decrypt(v)
				if err != nil {
					return fmt.Errorf("%s.%s: %w", table, col, err)
				}
				enc, err := secret.Encrypt(plain)
				if err != nil {
					return err
				}
				if _, err := tx.Exec(fmt.Sprintf(`UPDATE %s SET %s = ? WHERE rowid = ?`, table, col), enc, id); err != nil {
					return err
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.rewrapSealedFiles()
	return secret.FinishRotation(s.keyPath)
}

// rewrapSealedFiles moves the database snapshots next to the database
// (data/snapshots, sealed by ops) to the new key while both keys are
// loaded. A file that fails only costs that undo, not the rotation.
func (s *Store) rewrapSealedFiles() {
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(s.keyPath), "snapshots", "*.enc"))
	for _, f := range files {
		if err := secret.RewrapFile(f); err != nil {
			fmt.Println("master key rotation: snapshot", filepath.Base(f), "can't be read after it:", err)
		}
	}
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

// FailRunningDeployLogs marks the deploys a restart cut short as failed.
// The output is encrypted, so the note is appended here, not in SQL.
func (s *Store) FailRunningDeployLogs(ctx context.Context) error {
	logs, err := s.ListRunningDeployLogs(ctx)
	if err != nil {
		return err
	}
	for _, l := range logs {
		out := string(l.Output) + "\ninterrupted: agent restarted"
		if err := s.UpdateDeployLog(ctx, UpdateDeployLogParams{ID: l.ID, Status: "failed", Output: secret.String(out)}); err != nil {
			return err
		}
	}
	return nil
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
	for _, scope := range []string{"app", "worker"} {
		if err := q.DeleteSealedVarsOf(ctx, DeleteSealedVarsOfParams{Scope: scope, Owner: name}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Owner returns the owner's GitHub login, "" before anyone has claimed the panel.
func (s *Store) Owner(ctx context.Context) (string, error) {
	login, err := s.GetOwner(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return login, err
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
		_ = s.DeleteSession(ctx, sess.ID) // PruneOldData removes it otherwise
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
