package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store/teldb"
)

// A panel database from before telemetry.db hands its last month of
// telemetry over, still readable, and drops it.
func TestTelemetryMovesOut(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "hakobu.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Back to schema 7, which kept telemetry in the panel's database.
	if _, err := s.db.Exec(`CREATE TABLE telemetry_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT, app_name TEXT NOT NULL, kind TEXT NOT NULL,
		level TEXT NOT NULL DEFAULT '', message TEXT NOT NULL DEFAULT '', payload TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')));
		PRAGMA user_version = 7`); err != nil {
		t.Fatal(err)
	}
	enc := func(v string) string {
		e, err := secret.Encrypt(v)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	for _, e := range []struct{ msg, at string }{
		{"boom", timestamp(time.Now().Add(-time.Hour))},
		{"ancient", timestamp(time.Now().AddDate(0, -2, 0))},
	} {
		if _, err := s.db.Exec(`INSERT INTO telemetry_events (app_name, kind, message, created_at) VALUES ('web', 'error', ?, ?)`, enc(e.msg), e.at); err != nil {
			t.Fatal(err)
		}
	}
	removeTelemetry(filepath.Join(dir, telemetryFile))

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	events, err := s.Tel.ListTelemetryEvents(ctx, teldb.ListTelemetryEventsParams{AppName: "web", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Message != "boom" {
		t.Errorf("moved %+v, want the recent error only", events)
	}
	var has bool
	if err := s.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE name = 'telemetry_events')`).Scan(&has); err != nil || has {
		t.Errorf("the panel's database still has telemetry_events (err %v)", err)
	}
}

// A telemetry.db hakobu can't use is replaced: one from a newer hakobu, or
// one sealed with a master key that's gone.
func TestTelemetryStartsOver(t *testing.T) {
	dir := t.TempDir()
	fill := func() {
		db, err := openTelemetry(dir, false)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(`INSERT INTO telemetry_events (app_name, kind) VALUES ('web', 'error')`); err != nil {
			t.Fatal(err)
		}
	}
	empty := func(db *sql.DB, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM telemetry_events`).Scan(&n); err != nil || n != 0 {
			t.Errorf("%d events kept (err %v), want a new file", n, err)
		}
	}

	fill()
	db, err := sql.Open("sqlite", filepath.Join(dir, telemetryFile))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	empty(openTelemetry(dir, false))

	fill()
	empty(openTelemetry(dir, true))
}

func TestRotationReencryptsTelemetry(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tel.CreateTelemetryEvent(ctx, teldb.CreateTelemetryEventParams{AppName: "web", Kind: "error", Message: "boom"}); err != nil {
		t.Fatal(err)
	}
	var before string
	if err := s.telDB.QueryRow(`SELECT message FROM telemetry_events`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := s.RotateMasterKey(); err != nil {
		t.Fatal(err)
	}
	var after string
	if err := s.telDB.QueryRow(`SELECT message FROM telemetry_events`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Error("the telemetry is still sealed with the old key")
	}
	if plain, err := secret.Decrypt(after); err != nil || plain != "boom" {
		t.Errorf("after the rotation: %q, %v", plain, err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, telemetryFile)); err != nil {
		t.Error(err)
	}
}
