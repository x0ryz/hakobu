// Package tunnel runs the account's Cloudflare Tunnel as a cloudflared child
// process and restarts it if it exits.
package tunnel

import (
	"bufio"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type Runner struct {
	mu  sync.Mutex
	cmd *exec.Cmd
	gen int // bumped on every start so the previous restart loop exits
}

// Named runs the account's tunnel with its token, replacing any running tunnel.
func (r *Runner) Named(token string) {
	args := []string{"tunnel", "--no-autoupdate", "run", "--token", token}
	r.mu.Lock()
	r.gen++
	gen := r.gen
	if r.cmd != nil && r.cmd.Process != nil {
		r.cmd.Process.Kill()
	}
	r.mu.Unlock()

	go func() {
		for {
			cmd := exec.Command("cloudflared", args...)
			stderr, _ := cmd.StderrPipe()
			r.mu.Lock()
			if r.gen != gen {
				r.mu.Unlock()
				return
			}
			r.cmd = cmd
			err := cmd.Start()
			r.mu.Unlock()
			if err != nil {
				fmt.Println("tunnel: failed to start cloudflared:", err)
			} else {
				logErrors(stderr)
				cmd.Wait()
			}
			time.Sleep(3 * time.Second)
		}
	}()
}

func logErrors(r io.Reader) {
	s := bufio.NewScanner(r)
	for s.Scan() {
		// "context canceled" is a visitor closing the page mid-request.
		if line := s.Text(); strings.Contains(line, " ERR ") && !strings.Contains(line, "context canceled") {
			fmt.Println("cloudflared:", line)
		}
	}
}
