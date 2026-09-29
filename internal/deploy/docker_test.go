package deploy

import (
	"context"
	"os"
	"os/exec"
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
	got := make(chan [2]string, 1)
	go WatchOOM(ctx, func(container, app string) { got <- [2]string{container, app} })
	time.Sleep(time.Second) // let the event stream connect

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
}
