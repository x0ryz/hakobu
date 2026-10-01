package backup

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDockerBackup runs its own Postgres container:
// HAKOBU_DOCKER_TEST=1 go test ./internal/backup -run Docker -v
func TestDockerBackup(t *testing.T) {
	if os.Getenv("HAKOBU_DOCKER_TEST") == "" {
		t.Skip("set HAKOBU_DOCKER_TEST=1 to run against the local Docker")
	}
	ctx := context.Background()
	pg := "zt-backup-pg"
	_ = exec.Command("docker", "rm", "-f", pg).Run() // may not exist
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", pg).Run() })
	docker(t, "run", "-d", "--name", pg, "-e", "POSTGRES_PASSWORD=x", "postgres:18")
	waitFor(t, func() bool {
		return exec.Command("docker", "exec", pg, "pg_isready", "-h", "127.0.0.1", "-U", "postgres").Run() == nil
	})
	for _, sql := range []string{
		`CREATE USER app WITH PASSWORD 'x'`, `CREATE DATABASE main OWNER app`, `CREATE DATABASE copy OWNER app`,
	} {
		docker(t, "exec", pg, "psql", "-U", "postgres", "-c", sql)
	}
	docker(t, "exec", pg, "psql", "-U", "app", "-d", "main", "-c", `CREATE TABLE a (n int); CREATE TABLE b (s text); INSERT INTO a SELECT generate_series(1, 1000)`)

	// Dump through a file, restore elsewhere.
	f, err := os.Create(filepath.Join(t.TempDir(), "main.dump"))
	if err != nil {
		t.Fatal(err)
	}
	if err := DumpDatabase(ctx, pg, "app", "main", f); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if err := RestoreDatabase(ctx, pg, "app", "copy", f); err != nil {
		t.Fatal(err)
	}
	if n, err := CountTables(ctx, pg, "app", "copy"); err != nil || n != 2 {
		t.Errorf("restored %d tables, %v", n, err)
	}
	if rows := docker(t, "exec", pg, "psql", "-U", "app", "-d", "copy", "-Atc", "SELECT count(*) FROM a"); rows != "1000" {
		t.Errorf("restored %s rows", rows)
	}
	// A dump that fails halfway changes nothing and says why: this one
	// creates c, then clashes with the a restored above.
	docker(t, "exec", pg, "psql", "-U", "app", "-d", "main", "-c", `CREATE TABLE c (n int)`)
	var clash bytes.Buffer
	if err := DumpDatabase(ctx, pg, "app", "main", &clash); err != nil {
		t.Fatal(err)
	}
	err = RestoreDatabase(ctx, pg, "app", "copy", &clash)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("clashing restore error = %v", err)
	}
	if n, _ := CountTables(ctx, pg, "app", "copy"); n != 2 {
		t.Errorf("a failed restore left %d tables, want 2", n)
	}
	// psql meta-commands, which a plain SQL dump could carry, aren't run.
	err = RestoreDatabase(ctx, pg, "app", "copy", strings.NewReader("\\! touch /tmp/pwned\n"))
	if err == nil {
		t.Error("a plain SQL script was restored")
	}
	if exec.Command("docker", "exec", pg, "test", "-e", "/tmp/pwned").Run() == nil {
		t.Error("a meta-command in the dump ran a shell")
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(time.Minute); !ok(); time.Sleep(500 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
	}
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
