package store

import (
	"context"
	"io/fs"
	"os"
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
	if owner, _ := s.Owner(ctx); owner != "me" {
		t.Errorf("Owner = %q", owner)
	}
	if err := s.SetOwner(ctx, "someone-else"); err == nil {
		t.Error("the panel can only be claimed once")
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
	var raw string
	if err := s.db.QueryRow(`SELECT shared_env FROM projects`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !secret.IsEncrypted(raw) || strings.Contains(raw, "hunter2") {
		t.Errorf("stored %q, want ciphertext", raw)
	}
	// A reopened store (same master key) reads it back.
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	if p, err := s.GetProject(ctx, "demo"); err != nil || p.SharedEnv != "TOKEN=hunter2" {
		t.Errorf("read %q, %v", p.SharedEnv, err)
	}
	// A secret column holding something unencrypted is an error, not data.
	if _, err := s.db.Exec(`UPDATE projects SET shared_env = 'X=1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetProject(ctx, "demo"); err == nil {
		t.Error("plaintext in a secret column was accepted")
	}

	if err := s.NewSession(ctx, "tok", "me", time.Hour); err != nil {
		t.Fatal(err)
	}
	if login, err := s.SessionLogin(ctx, "tok"); err != nil || login != "me" {
		t.Errorf("session = %q, %v", login, err)
	}
	var stored string
	if err := s.db.QueryRow(`SELECT id FROM sessions`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == "tok" {
		t.Error("session token stored as is")
	}
}

func TestRotateMasterKey(t *testing.T) {
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
	if err := s.SetSealedVar(ctx, SetSealedVarParams{Scope: "app", Owner: "web", Key: "K", Value: "sealed"}); err != nil {
		t.Fatal(err)
	}
	raw := func() string {
		var v string
		if err := s.db.QueryRow(`SELECT shared_env FROM projects`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	keyFile := filepath.Join(dir, "master.key")
	oldKey, _ := os.ReadFile(keyFile)
	before := raw()

	if err := s.RotateMasterKey(); err != nil {
		t.Fatal(err)
	}
	newKey, _ := os.ReadFile(keyFile)
	if string(newKey) == string(oldKey) || raw() == before {
		t.Error("the key and the stored value should both change")
	}
	if _, err := os.Stat(keyFile + ".new"); !os.IsNotExist(err) {
		t.Error("master.key.new left behind")
	}
	if p, err := s.GetProject(ctx, "demo"); err != nil || p.SharedEnv != "TOKEN=hunter2" {
		t.Errorf("after rotation: %q, %v", p.SharedEnv, err)
	}
	if v, err := s.ListSealedVars(ctx, ListSealedVarsParams{Scope: "app", Owner: "web"}); err != nil || v[0].Value != "sealed" {
		t.Errorf("sealed var after rotation: %v, %v", v, err)
	}

	// A rotation cut short (new key written, nothing re-encrypted) is
	// finished by the next Open.
	if err := secret.BeginRotation(keyFile); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	if secret.Rotating() {
		t.Error("Open didn't finish the rotation")
	}
	if _, err := os.Stat(keyFile + ".new"); !os.IsNotExist(err) {
		t.Error("master.key.new left behind")
	}
	if p, err := s.GetProject(ctx, "demo"); err != nil || p.SharedEnv != "TOKEN=hunter2" {
		t.Errorf("after an interrupted rotation: %q, %v", p.SharedEnv, err)
	}
}

// TestSecretColumnsMatchSqlc: every column sqlc treats as a secret must be
// re-encrypted by a key rotation.
func TestSecretColumnsMatchSqlc(t *testing.T) {
	b, err := os.ReadFile("../../sqlc.yaml")
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for table, cols := range secretColumns {
		for _, c := range cols {
			listed[table+"."+c] = true
		}
	}
	lines := strings.Split(string(b), "\n")
	for i, line := range lines {
		col, ok := strings.CutPrefix(strings.TrimSpace(line), "- column: ")
		if !ok || i+1 >= len(lines) || !strings.Contains(lines[i+1], "type: String") || strings.HasPrefix(col, "app_view.") {
			continue // app_view is a view over apps
		}
		if !listed[col] {
			t.Errorf("%s is a secret in sqlc.yaml but not in secretColumns", col)
		}
		delete(listed, col)
	}
	for col := range listed {
		t.Errorf("%s is in secretColumns but not a secret in sqlc.yaml", col)
	}
}
