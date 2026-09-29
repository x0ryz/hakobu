package store

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/x0ryz/hakobu/internal/secret"
)

func TestStore(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "hakobu.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.CreateProject(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	p, err := s.GetProject(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"web", "api"} {
		if err := s.CreateApp(ctx, CreateAppParams{ProjectID: p.ID, Name: name, Repo: "o/r", ContainerPort: 8080, BuildStrategy: "railpack"}); err != nil {
			t.Fatal(err)
		}
	}
	web, err := s.GetApp(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	api, _ := s.GetApp(ctx, "api")
	if web.Port != 8081 || api.Port != 8082 {
		t.Errorf("ports = %d, %d, want 8081, 8082", web.Port, api.Port)
	}
	if web.ProjectName != "demo" || web.ContainerName() != "web-blue" {
		t.Errorf("app view: project %q, container %q", web.ProjectName, web.ContainerName())
	}
	if apps, _ := s.ListAppsByRepo(ctx, "o/r"); len(apps) != 2 {
		t.Errorf("ListAppsByRepo = %d apps, want 2", len(apps))
	}

	if err := s.SaveWorker(ctx, SaveWorkerParams{AppName: "web", Name: "w", Command: "run"}); err != nil {
		t.Fatal(err)
	}
	id, err := s.CreateDeployLog(ctx, CreateDeployLogParams{AppName: "web", Trigger: "manual", Status: "running"})
	if err != nil || id == 0 {
		t.Fatalf("CreateDeployLog = %d, %v", id, err)
	}
	if err := s.FailRunningDeployLogs(ctx); err != nil {
		t.Fatal(err)
	}
	logs, _ := s.ListDeployLogs(ctx, ListDeployLogsParams{AppName: "web", Limit: 10})
	if len(logs) != 1 || logs[0].Status != "failed" {
		t.Errorf("deploy logs after restart = %+v", logs)
	}
	if err := s.DeleteAppCascade(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetWorker(ctx, "web"); err == nil {
		t.Error("worker survived app deletion")
	}
	if logs, _ := s.ListDeployLogs(ctx, ListDeployLogsParams{AppName: "web", Limit: 10}); len(logs) != 0 {
		t.Error("deploy logs survived app deletion")
	}

	if owner, err := s.Owner(ctx); err != nil || owner != "" {
		t.Errorf("Owner = %q, %v before setup", owner, err)
	}
	if err := s.SetOwner(ctx, "me"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAllowedLogins(ctx, " a, ,b "); err != nil {
		t.Fatal(err)
	}
	if owner, _ := s.Owner(ctx); owner != "me" {
		t.Errorf("Owner = %q", owner)
	}
	if allowed, _ := s.AllowedLogins(ctx); len(allowed) != 2 || allowed[0] != "a" || allowed[1] != "b" {
		t.Errorf("AllowedLogins = %q", allowed)
	}

	if err := s.NewSession(ctx, "live", "me", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.NewSession(ctx, "old", "me", -time.Hour); err != nil {
		t.Fatal(err)
	}
	if login, err := s.SessionLogin(ctx, "live"); err != nil || login != "me" {
		t.Errorf("live session = %q, %v", login, err)
	}
	if _, err := s.SessionLogin(ctx, "old"); err == nil {
		t.Error("expired session accepted")
	}
	if err := s.PruneOldData(ctx, 7); err != nil {
		t.Fatal(err)
	}

	// Reopening must not re-run applied migrations.
	s.db.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	var version int
	s.db.QueryRow(`PRAGMA user_version`).Scan(&version)
	if files, _ := fs.Glob(migrationFiles, "migrations/*.sql"); version != len(files) {
		t.Errorf("user_version = %d, want %d", version, len(files))
	}
	if _, err := s.GetApp(ctx, "api"); err != nil {
		t.Errorf("data lost on reopen: %v", err)
	}
}

// A database created by an older hakobu keeps its data when newer migrations run.
func TestMigrateFromVersion1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hakobu.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	v1, _ := migrationFiles.ReadFile("migrations/001_init.sql")
	for _, q := range []string{string(v1), `PRAGMA user_version = 1`,
		`INSERT INTO projects (name) VALUES ('demo')`,
		`INSERT INTO apps (project_id, name, port, container_port) VALUES (1, 'web', 8081, 8000)`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	app, err := s.GetApp(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	if app.ContainerPort != 8000 || app.LivePort != 0 || app.ProjectName != "demo" {
		t.Errorf("migrated app = %+v", app)
	}
}

func TestSecretsEncryptedAtRest(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "hakobu.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProject(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProjectSharedEnv(ctx, SetProjectSharedEnvParams{Name: "demo", SharedEnv: "TOKEN=hunter2"}); err != nil {
		t.Fatal(err)
	}
	// A value from before encryption is encrypted on the next Open.
	if _, err := s.db.Exec(`INSERT INTO projects (name, shared_env) VALUES ('old', 'OLD=plain')`); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}

	rows, err := s.db.Query(`SELECT name, shared_env FROM projects`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, raw string
		rows.Scan(&name, &raw)
		if !secret.IsEncrypted(raw) || strings.Contains(raw, "hunter2") || strings.Contains(raw, "plain") {
			t.Errorf("%s: stored %q, want ciphertext", name, raw)
		}
	}
	for name, want := range map[string]string{"demo": "TOKEN=hunter2", "old": "OLD=plain"} {
		p, err := s.GetProject(ctx, name)
		if err != nil || string(p.SharedEnv) != want {
			t.Errorf("%s: read %q, %v, want %q", name, p.SharedEnv, err, want)
		}
	}

	if err := s.NewSession(ctx, "tok", "me", time.Hour); err != nil {
		t.Fatal(err)
	}
	if login, err := s.SessionLogin(ctx, "tok"); err != nil || login != "me" {
		t.Errorf("session = %q, %v", login, err)
	}
	var stored string
	s.db.QueryRow(`SELECT id FROM sessions`).Scan(&stored)
	if stored == "tok" {
		t.Error("session token stored as is")
	}
}
