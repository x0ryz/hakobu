package deploy

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"testing"
	"time"
)

// HAKOBU_DOCKER_TEST=1 go test ./internal/deploy -run Docker -v
func TestDockerWatchOOM(t *testing.T) {
	if os.Getenv("HAKOBU_DOCKER_TEST") == "" {
		t.Skip("set HAKOBU_DOCKER_TEST=1 to run against the local Docker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	got := make(chan [2]string, 4)
	go func() { _ = WatchOOM(ctx, func(container, app string, _ bool) { got <- [2]string{container, app} }) }() // ends with ctx
	time.Sleep(time.Second)                                                                                     // let the event stream connect

	// Docker's own SIGKILL (docker kill, a stop that timed out) isn't one.
	_ = exec.Command("docker", "rm", "-f", "zt-killed").Run()
	if out, err := exec.Command("docker", "run", "-d", "--name", "zt-killed", "busybox:1.36", "sleep", "60").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "zt-killed").Run() })
	if out, err := exec.Command("docker", "kill", "zt-killed").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}

	// Under rootless Docker the kill often comes without an "oom" event;
	// either way it's reported.
	out, err := exec.Command("docker", "run", "--rm", "--name", "zt-oom", "-m", "32m", "--memory-swap", "32m",
		"--label", AppLabel+"=zt-app", "busybox:1.36", "dd", "if=/dev/zero", "of=/dev/null", "bs=256M", "count=1").CombinedOutput()
	if err == nil {
		t.Fatalf("container should have been killed:\n%s", out)
	}
	select {
	case ev := <-got:
		if ev != [2]string{"zt-oom", "zt-app"} {
			t.Errorf("event = %v", ev)
		}
	case <-ctx.Done():
		t.Fatal("no oom event")
	}
	time.Sleep(2 * oomEventGrace) // a double report would come by now
	if len(got) > 0 {
		t.Errorf("reported more than once: %v", <-got)
	}
}

func TestAppContainersDropRawSockets(t *testing.T) {
	_, hostConfig := AppOptions{App: "web", Network: "hakobu_p"}.spec("img")
	if drop, _ := hostConfig["CapDrop"].([]string); !slices.Contains(drop, "NET_RAW") {
		t.Errorf("CapDrop = %v", hostConfig["CapDrop"])
	}
}
