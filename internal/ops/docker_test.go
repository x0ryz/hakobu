package ops

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/x0ryz/hakobu/internal/deploy"
	"github.com/x0ryz/hakobu/internal/store"
)

// TestDockerDeploys runs real containers: HAKOBU_DOCKER_TEST=1 go test ./internal/ops -run Docker -v
func TestDockerDeploys(t *testing.T) {
	if os.Getenv("HAKOBU_DOCKER_TEST") == "" {
		t.Skip("set HAKOBU_DOCKER_TEST=1 to run against the local Docker")
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	app := newDockerTestApp(t, s)
	t.Cleanup(func() { DeleteApp(s, app.Name) })

	reload := func() store.App {
		a, err := s.GetApp(ctx(), app.Name)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	deployVersion := func(v string, wantErr bool) string {
		t.Helper()
		buildTestImage(t, nextImageTag(app), v)
		var out strings.Builder
		err := rollOut(s, reload(), nextImageTag(app), &out)
		if wantErr {
			if err == nil {
				t.Fatalf("deploy %s should fail:\n%s", v, out.String())
			}
			return out.String()
		}
		if err != nil {
			t.Fatalf("deploy %s: %v\n%s", v, err, out.String())
		}
		if err := promote(reload(), &out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}

	if err := AddVolume(s, app.Name, "data", "/data"); err != nil {
		t.Fatal(err)
	}

	// First deploy, port detected automatically.
	deployVersion("v1", false)
	expectServing(t, app, "v1")
	if a := reload(); a.LivePort != 8080 {
		t.Errorf("detected port %d, want 8080", a.LivePort)
	}
	v1 := deploy.ImageID(ctx(), ImageTag(app))

	// Volumes: recreate mode, data survives.
	out := deployVersion("v2", false)
	if !strings.Contains(out, "stopping") {
		t.Errorf("app with volumes should stop the old version first:\n%s", out)
	}
	expectServing(t, app, "v2")
	if got := dataLines(t, reload()); got != 2 {
		t.Errorf("volume has %d start entries, want 2 (data lost between deploys?)", got)
	}
	if n := countContainers(t, app.Name+"-"); n != 1 {
		t.Errorf("%d app containers after deploy, want 1", n)
	}
	if deploy.ImageID(ctx(), PreviousImageTag(app)) != v1 {
		t.Error(":previous should be the v1 image")
	}

	// The worker mounts the same volume.
	if err := SaveWorker(s, store.Worker{AppName: app.Name, Name: "w", Command: "sleep 3600"}); err != nil {
		t.Fatal(err)
	}
	if m := dockerOut(t, "inspect", "-f", "{{range .Mounts}}{{.Name}}{{end}}", app.Name+"-worker"); m != dockerVolume(app.Name, "data") {
		t.Errorf("worker mounts %q", m)
	}

	// A third build drops v1: only :latest and :previous are kept.
	deployVersion("v3", false)
	if deploy.ImageID(ctx(), v1) != "" {
		t.Error("the image that fell out of the rotation should be deleted")
	}

	// A broken build is rejected and the old version is started again.
	out = deployVersion("broken", true)
	if !strings.Contains(out, "started the previous version again") {
		t.Errorf("expected the old version to be restarted:\n%s", out)
	}
	expectServing(t, app, "v3")

	// Shared volumes keep the zero-downtime switch.
	if err := SetShareVolumes(s, app.Name, true); err != nil {
		t.Fatal(err)
	}
	if out := deployVersion("v4", false); strings.Contains(out, "stopping") {
		t.Errorf("shared volumes should not stop the old version:\n%s", out)
	}
	expectServing(t, app, "v4")

	// Rollback goes to v3, rolling back again returns to v4.
	for _, want := range []string{"v3", "v4"} {
		if err := StartRollback(s, app.Name); err != nil {
			t.Fatal(err)
		}
		waitIdle(t, app.Name)
		expectServing(t, app, want)
	}

	// Deleting the app removes containers, images and volumes.
	if err := DeleteApp(s, app.Name); err != nil {
		t.Fatal(err)
	}
	if n := countContainers(t, app.Name+"-"); n != 0 {
		t.Errorf("%d containers left after delete", n)
	}
	if ok, _ := deploy.ImageExists(ctx(), ImageTag(app)); ok {
		t.Error("image left after delete")
	}
	if vols, _ := deploy.VolumeNames(ctx(), dockerVolume(app.Name, "")); len(vols) != 0 {
		t.Errorf("volumes left after delete: %v", vols)
	}
}

// newDockerTestApp creates an app whose proxy port is free on this machine
// (a real hakobu may be using the first ones).
func newDockerTestApp(t *testing.T, s *store.Store) store.App {
	suffix, _ := RandomHex(3)
	project := "zt" + suffix
	if err := s.CreateProject(ctx(), project); err != nil {
		t.Fatal(err)
	}
	p, _ := s.GetProject(ctx(), project)
	for i := 0; ; i++ {
		name := fmt.Sprintf("zt%s-%d", suffix, i)
		if err := s.CreateApp(ctx(), store.CreateAppParams{ProjectID: p.ID, Name: name, BuildStrategy: "dockerfile"}); err != nil {
			t.Fatal(err)
		}
		app, _ := s.GetApp(ctx(), name)
		if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", app.Port)); err == nil {
			ln.Close()
			return app
		}
	}
}

func buildTestImage(t *testing.T, tag, version string) {
	t.Helper()
	cmd := `mkdir -p /data && date >> /data/log && exec httpd -f -p 8080 -h /www`
	if version == "broken" {
		cmd = "sleep 3600" // never listens
	}
	dir := t.TempDir()
	dockerfile := fmt.Sprintf("FROM busybox:1.36\nRUN mkdir /www && echo -n %s > /www/index.html\nCMD [\"sh\", \"-c\", %q]\n", version, cmd)
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	dockerOut(t, "build", "-q", "-t", tag, dir)
}

func expectServing(t *testing.T, app store.App, want string) {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", app.Port))
	if err != nil {
		t.Fatalf("app proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != want {
		t.Errorf("serving %q, want %q", body, want)
	}
}

func dataLines(t *testing.T, app store.App) int {
	return len(strings.Split(strings.TrimSpace(dockerOut(t, "exec", app.ContainerName(), "cat", "/data/log")), "\n"))
}

func countContainers(t *testing.T, prefix string) int {
	n := 0
	for _, name := range strings.Fields(dockerOut(t, "ps", "-a", "--format", "{{.Names}}")) {
		if strings.HasPrefix(name, prefix) {
			n++
		}
	}
	return n
}

func waitIdle(t *testing.T, app string) {
	for deadline := time.Now().Add(2 * time.Minute); IsDeploying(app); time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("job still running")
		}
	}
}

func dockerOut(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
