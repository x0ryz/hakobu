package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store"
)

// TestDockerBackup runs its own Postgres and RustFS containers:
// HAKOBU_DOCKER_TEST=1 go test ./internal/backup -run Docker -v
func TestDockerBackup(t *testing.T) {
	if os.Getenv("HAKOBU_DOCKER_TEST") == "" {
		t.Skip("set HAKOBU_DOCKER_TEST=1 to run against the local Docker")
	}
	ctx := context.Background()
	pg, fs := "zt-backup-pg", "zt-backup-rustfs"
	for _, c := range []string{pg, fs} {
		exec.Command("docker", "rm", "-f", c).Run()
		t.Cleanup(func() { exec.Command("docker", "rm", "-f", c).Run() })
	}
	docker(t, "run", "-d", "--name", pg, "-e", "POSTGRES_PASSWORD=x", "postgres:18")
	docker(t, "run", "-d", "--name", fs, "-e", "RUSTFS_ACCESS_KEY=ztaccess", "-e", "RUSTFS_SECRET_KEY=ztsecret123",
		"-e", "RUSTFS_ADDRESS=:9000", "-e", "RUSTFS_CONSOLE_ENABLE=false", "rustfs/rustfs:latest")

	ip := docker(t, "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", fs)
	client := NewClient(store.Storage{Endpoint: "http://" + ip + ":9000", Bucket: "zt-backup", AccessKeyID: "ztaccess", SecretAccessKey: secret.String("ztsecret123")})
	waitFor(t, func() bool { return client.CreateBucket() == nil }) // 503 until RustFS is up
	waitFor(t, func() bool {
		return exec.Command("docker", "exec", pg, "pg_isready", "-h", "127.0.0.1", "-U", "postgres").Run() == nil
	})
	for _, sql := range []string{
		`CREATE USER app WITH PASSWORD 'x'`, `CREATE DATABASE main OWNER app`, `CREATE DATABASE copy OWNER app`,
	} {
		docker(t, "exec", pg, "psql", "-U", "postgres", "-c", sql)
	}
	docker(t, "exec", pg, "psql", "-U", "app", "-d", "main", "-c", `CREATE TABLE a (n int); CREATE TABLE b (s text); INSERT INTO a SELECT generate_series(1, 1000)`)

	// Dump through a file, upload, download, restore elsewhere.
	f, err := os.Create(filepath.Join(t.TempDir(), "dump.sql.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if err := DumpDatabase(ctx, pg, "app", "main", f); err != nil {
		t.Fatal(err)
	}
	if err := client.PutFile("backups/main/1.sql.gz", f, "application/gzip"); err != nil {
		t.Fatal(err)
	}
	body, err := client.GetStream("backups/main/1.sql.gz")
	if err != nil {
		t.Fatal(err)
	}
	if err := RestoreDatabase(ctx, pg, "app", "copy", body); err != nil {
		t.Fatal(err)
	}
	body.Close()
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

	// Files over partSize go up in parts and come back identical.
	big, err := os.Create(filepath.Join(t.TempDir(), "big"))
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.New()
	if _, err := io.CopyN(io.MultiWriter(big, want), rand.Reader, partSize+partSize/2); err != nil {
		t.Fatal(err)
	}
	if err := client.PutFile("big", big, ""); err != nil {
		t.Fatal(err)
	}
	body, err = client.GetStream("big")
	if err != nil {
		t.Fatal(err)
	}
	got := sha256.New()
	io.Copy(got, body)
	body.Close()
	if !bytes.Equal(got.Sum(nil), want.Sum(nil)) {
		t.Error("multipart upload came back different")
	}
	for _, key := range []string{"big", "backups/main/1.sql.gz"} {
		if err := client.DeleteObject(key); err != nil {
			t.Error(err)
		}
	}
	if _, err := client.GetStream("big"); err == nil {
		t.Error("deleted object still there")
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
