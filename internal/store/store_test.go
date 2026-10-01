package store

import (
	"context"
	"io"
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

	if owner, err := s.Owner(ctx); err != nil || owner.GitHubID != 0 {
		t.Errorf("Owner = %+v, %v before setup", owner, err)
	}
	if err := s.SetOwner(ctx, SetOwnerParams{GitHubID: 42, GitHubLogin: "me"}); err != nil {
		t.Fatal(err)
	}
	if owner, _ := s.Owner(ctx); owner.GitHubID != 42 || owner.GitHubLogin != "me" {
		t.Errorf("Owner = %+v", owner)
	}
	if err := s.SetOwner(ctx, SetOwnerParams{GitHubID: 7, GitHubLogin: "someone-else"}); err == nil {
		t.Error("the panel can only be claimed once")
	}

	if err := s.NewSession(ctx, "live", 42, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.NewSession(ctx, "old", 42, -time.Hour); err != nil {
		t.Fatal(err)
	}
	if id, err := s.SessionUser(ctx, "live"); err != nil || id != 42 {
		t.Errorf("live session = %d, %v", id, err)
	}
	if _, err := s.SessionUser(ctx, "old"); err == nil {
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

	if err := s.NewSession(ctx, "tok", 42, time.Hour); err != nil {
		t.Fatal(err)
	}
	if id, err := s.SessionUser(ctx, "tok"); err != nil || id != 42 {
		t.Errorf("session = %d, %v", id, err)
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

// Logs, errors and worker commands are stored encrypted like the other
// secrets, and a deploy cut short by a restart is still closed with a note.
func TestLogsEncryptedAtRest(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProject(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	p, _ := s.GetProject(ctx, "demo")
	if err := s.CreateApp(ctx, CreateAppParams{ProjectID: p.ID, Name: "web", Repo: "o/r", BuildStrategy: "railpack"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTelemetryEvent(ctx, CreateTelemetryEventParams{AppName: "web", Kind: "error", Level: "error", Message: "user alice@example.com", Payload: `{"ip":"203.0.113.9"}`}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveWorker(ctx, SaveWorkerParams{AppName: "web", Name: "w", Command: "worker --token=hunter2"}); err != nil {
		t.Fatal(err)
	}
	id, err := s.CreateDeployLog(ctx, CreateDeployLogParams{AppName: "web", Trigger: "push", Status: "running"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateDeployLog(ctx, UpdateDeployLogParams{ID: id, Status: "running", Output: "building with TOKEN=hunter2"}); err != nil {
		t.Fatal(err)
	}

	for _, q := range []string{
		`SELECT message || payload FROM telemetry_events`,
		`SELECT command FROM workers`,
		`SELECT output FROM deploy_logs`,
	} {
		var raw string
		if err := s.db.QueryRow(q).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(raw, "hunter2") || strings.Contains(raw, "alice") || strings.Contains(raw, "203.0.113") {
			t.Errorf("%s: plaintext stored: %q", q, raw)
		}
	}

	if err := s.FailRunningDeployLogs(ctx); err != nil {
		t.Fatal(err)
	}
	logs, err := s.ListDeployLogs(ctx, ListDeployLogsParams{AppName: "web", Limit: 1})
	if err != nil || len(logs) != 1 {
		t.Fatal(logs, err)
	}
	if logs[0].Status != "failed" || string(logs[0].Output) != "building with TOKEN=hunter2\ninterrupted: agent restarted" {
		t.Errorf("interrupted deploy = %s %q", logs[0].Status, logs[0].Output)
	}
}

func TestMissingKeyWithSecrets(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "hakobu.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkKeyNotLost(s.db, "data/master.key", true); err != nil {
		t.Errorf("an empty database needs no old key: %v", err)
	}
	if err := s.SaveCloudflareToken(ctx, "tok"); err != nil {
		t.Fatal(err)
	}
	err = checkKeyNotLost(s.db, "data/master.key", true)
	if err == nil || !strings.Contains(err.Error(), "cloudflare.api_token") {
		t.Errorf("a missing key over secrets: %v, want a refusal naming the column", err)
	}
	if err := checkKeyNotLost(s.db, "data/master.key", false); err != nil {
		t.Errorf("key present: %v", err)
	}
}

// The database snapshots ops seals next to the database stay readable
// after the master key, kept in a directory of its own, is rotated.
func TestRotationRewrapsSnapshots(t *testing.T) {
	root := t.TempDir()
	dir, keyPath := filepath.Join(root, "data"), filepath.Join(root, "key", "master.key")
	for _, d := range []string{dir, filepath.Dir(keyPath)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	s, err := OpenWithKey(filepath.Join(dir, "hakobu.db"), keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Errorf("master key not where asked: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "master.key*")); len(left) != 0 {
		t.Errorf("master key in the database's directory: %v", left)
	}
	snap := filepath.Join(dir, "snapshots", "web.dump.enc")
	if err := os.MkdirAll(filepath.Dir(snap), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(snap)
	if err != nil {
		t.Fatal(err)
	}
	w, err := secret.NewFileWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("dump")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if err := s.RotateMasterKey(); err != nil {
		t.Fatal(err)
	}
	f, err = os.Open(snap)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r, err := secret.NewFileReader(f)
	if err != nil {
		t.Fatalf("snapshot after rotation: %v", err)
	}
	if b, err := io.ReadAll(r); err != nil || string(b) != "dump" {
		t.Errorf("snapshot after rotation = %q, %v", b, err)
	}
}
