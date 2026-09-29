package s3

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store"
)

// HAKOBU_DOCKER_TEST=1 go test ./internal/s3 -run Docker -v
func TestDockerCreateBucket(t *testing.T) {
	if os.Getenv("HAKOBU_DOCKER_TEST") == "" {
		t.Skip("set HAKOBU_DOCKER_TEST=1 to run against the local Docker")
	}
	fs := "zt-s3-rustfs"
	exec.Command("docker", "rm", "-f", fs).Run()
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", fs).Run() })
	if out, err := exec.Command("docker", "run", "-d", "--name", fs, "-e", "RUSTFS_ACCESS_KEY=ztaccess", "-e", "RUSTFS_SECRET_KEY=ztsecret123",
		"-e", "RUSTFS_ADDRESS=:9000", "-e", "RUSTFS_CONSOLE_ENABLE=false", "rustfs/rustfs:latest").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	out, _ := exec.Command("docker", "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", fs).Output()
	st := store.Storage{Endpoint: "http://" + strings.TrimSpace(string(out)) + ":9000", Bucket: "zt-bucket", AccessKeyID: "ztaccess", SecretAccessKey: secret.String("ztsecret123")}

	// RustFS answers 503 until it's up; then the bucket is created, and
	// creating it again is fine.
	var err error
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		if err = NewClient(st).CreateBucket(); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := NewClient(st).CreateBucket(); err != nil {
		t.Errorf("second create: %v", err)
	}
	st.SecretAccessKey = "wrong"
	if err := NewClient(st).CreateBucket(); err == nil {
		t.Error("a wrong key was accepted")
	}
}
