package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// startTestDialer serves dial requests in-process, allowing only 127.0.0.1.
func startTestDialer(t *testing.T) *net.UnixConn {
	t.Helper()
	ours, theirs, err := seqpacketPair()
	if err != nil {
		t.Fatal(err)
	}
	control, err := unixConn(ours)
	if err != nil {
		t.Fatal(err)
	}
	server, err := unixConn(theirs)
	if err != nil {
		t.Fatal(err)
	}
	go serveDialer(server, func(ip net.IP) bool { return ip.Equal(net.IPv4(127, 0, 0, 1)) })
	t.Cleanup(func() { control.Close(); server.Close() })
	return control
}

func TestDialerHandsBackConnection(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(c, c)
	}()

	control := startTestDialer(t)
	conn, err := dialVia(context.Background(), control, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, ok := conn.(*net.TCPConn); !ok {
		t.Errorf("got %T, want a TCP connection", conn)
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Errorf("echo = %q, %v", buf, err)
	}
}

func TestDialerRefusals(t *testing.T) {
	control := startTestDialer(t)
	ctx := context.Background()

	for _, c := range []struct{ network, addr, want string }{
		{"tcp", "127.0.0.2:80", "not on a container network"},
		{"tcp", "1.1.1.1:443", "not on a container network"},
		{"udp", "127.0.0.1:53", "not allowed"},
	} {
		_, err := dialVia(ctx, control, c.network, c.addr)
		if err == nil || !strings.Contains(err.Error(), c.want) || errors.Is(err, errHelperGone) {
			t.Errorf("%s %s: %v, want %q", c.network, c.addr, err, c.want)
		}
	}

	// Nothing listening: the target's refusal comes back, not a dead helper.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	if _, err := dialVia(ctx, control, "tcp", addr); err == nil || errors.Is(err, errHelperGone) {
		t.Errorf("closed port: %v, want connection refused", err)
	}
}

func TestDialerGone(t *testing.T) {
	ours, theirs, err := seqpacketPair()
	if err != nil {
		t.Fatal(err)
	}
	control, err := unixConn(ours)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	theirs.Close() // the helper exited
	if _, err := dialVia(context.Background(), control, "tcp", "127.0.0.1:1"); !errors.Is(err, errHelperGone) {
		t.Errorf("got %v, want errHelperGone", err)
	}
}

// Concurrent dials each get the connection they asked for.
func TestDialerConcurrent(t *testing.T) {
	control := startTestDialer(t)
	var addrs []string
	for i := range 10 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		addrs = append(addrs, ln.Addr().String())
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				_, _ = fmt.Fprint(c, i)
				c.Close()
			}
		}()
	}

	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for n := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			i := n % len(addrs)
			conn, err := dialVia(context.Background(), control, "tcp", addrs[i])
			if err != nil {
				errs <- err
				return
			}
			defer conn.Close()
			got, _ := io.ReadAll(conn)
			if string(got) != fmt.Sprint(i) {
				errs <- fmt.Errorf("dial %s reached listener %q, want %d", addrs[i], got, i)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// Every path, including callers that give up, gives back its descriptors.
func TestDialerKeepsNoDescriptors(t *testing.T) {
	control := startTestDialer(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	refused := closed.Addr().String()
	closed.Close()

	openFds := func() int {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Skip("no /proc/self/fd")
		}
		return len(entries)
	}
	round := func() {
		var wg sync.WaitGroup
		for i := range 50 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), time.Duration(i%5)*100*time.Microsecond+time.Microsecond)
				defer cancel()
				for _, addr := range []string{ln.Addr().String(), refused, "10.255.255.1:80"} {
					if c, err := dialVia(ctx, control, "tcp", addr); err == nil {
						c.Close()
					}
					if c, err := dialVia(context.Background(), control, "tcp", addr); err == nil {
						c.Close()
					}
				}
			}()
		}
		wg.Wait()
	}
	round() // warm up the runtime's own descriptors
	before := openFds()
	for range 5 {
		round()
	}
	// The helper closes its side from goroutines; give them a moment.
	deadline := time.Now().Add(3 * time.Second)
	for openFds() > before && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if after := openFds(); after > before {
		t.Errorf("%d descriptors open after 750 dials, %d before", after, before)
	}
}

// A helper run by the Docker user is reached through its socket, and a
// restarted one (as when Docker restarts) is reconnected to.
func TestDialerSocket(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	path := filepath.Join(t.TempDir(), "dialer.sock")
	serve := func() {
		go func() {
			_ = serveDialerSocket(path, func(ip net.IP) bool { return ip.Equal(net.IPv4(127, 0, 0, 1)) })
		}()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(path); err == nil {
				return
			} else if time.Now().After(deadline) {
				t.Fatal("helper socket never appeared")
			}
		}
	}
	serve()
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o660 {
		t.Errorf("socket mode = %v, %v", fi.Mode().Perm(), err)
	}
	t.Setenv(dialerSocketEnv, path)
	h := &dialerHelper{}
	t.Cleanup(func() { h.mu.Lock(); h.stopLocked(); h.mu.Unlock() })
	conn, err := h.dial(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()

	// The helper's connection breaks, like when it restarts with Docker.
	h.mu.Lock()
	h.control.Close()
	h.mu.Unlock()
	if conn, err = h.dial(context.Background(), "tcp", ln.Addr().String()); err != nil {
		t.Fatalf("after a restart: %v", err)
	}
	conn.Close()
}
