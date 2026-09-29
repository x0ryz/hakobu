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
func TestDockerRustFSUsers(t *testing.T) {
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
	st.SecretAccessKey = "ztsecret123"
	root := NewClient(st)
	other := st
	other.Bucket = "zt-other"
	if err := NewClient(other).CreateBucket(); err != nil {
		t.Fatal(err)
	}

	// A bucket user (hakobu's own key lengths) reaches its bucket only.
	if err := root.AddBucketUser("hk0123456789abcdef01", strings.Repeat("s", 40)); err != nil {
		t.Fatal(err)
	}
	user := store.Storage{Endpoint: st.Endpoint, AccessKeyID: "hk0123456789abcdef01", SecretAccessKey: secret.String(strings.Repeat("s", 40))}
	user.Bucket = "zt-bucket"
	if err := NewClient(user).CreateBucket(); err != nil {
		t.Errorf("user on its own bucket: %v", err)
	}
	user.Bucket = "zt-other"
	if err := NewClient(user).CreateBucket(); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("user on another bucket: %v", err)
	}
	if err := NewClient(user).AddBucketUser("hkevil", "secretsecret"); err == nil {
		t.Error("a bucket user could use the admin API")
	}

	// Removing it revokes the keys; doing it twice is fine.
	for range 2 {
		if err := root.RemoveBucketUser("hk0123456789abcdef01"); err != nil {
			t.Fatal(err)
		}
	}
	user.Bucket = "zt-bucket"
	if err := NewClient(user).CreateBucket(); err == nil {
		t.Error("removed user still works")
	}
}
