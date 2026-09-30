package deploy

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
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
