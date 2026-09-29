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
	exec.Command("docker", "rm", "-f", pg).Run()
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", pg).Run() })
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
	f, err := os.Create(filepath.Join(t.TempDir(), "dump.sql.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if err := DumpDatabase(ctx, pg, "app", "main", f); err != nil {
		t.Fatal(err)
	}
	f.Seek(0, io.SeekStart)
	if err := RestoreDatabase(ctx, pg, "app", "copy", f); err != nil {
		t.Fatal(err)
	}
	if n, err := CountTables(ctx, pg, "app", "copy"); err != nil || n != 2 {
		t.Errorf("restored %d tables, %v", n, err)
	}
	if rows := docker(t, "exec", pg, "psql", "-U", "app", "-d", "copy", "-Atc", "SELECT count(*) FROM a"); rows != "1000" {
		t.Errorf("restored %s rows", rows)
	}
	// A broken dump changes nothing and says why.
	err = RestoreDatabase(ctx, pg, "app", "copy", gzipped("CREATE TABLE c (n int);\nSELECT nonsense;\n"))
	if err == nil || !strings.Contains(err.Error(), "nonsense") {
		t.Errorf("broken restore error = %v", err)
	}
	if n, _ := CountTables(ctx, pg, "app", "copy"); n != 2 {
		t.Errorf("a failed restore left %d tables, want 2", n)
	}
}

func gzipped(s string) io.Reader {
	var b bytes.Buffer
	cmd := exec.Command("gzip")
	cmd.Stdin, cmd.Stdout = strings.NewReader(s), &b
	cmd.Run()
	return &b
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
