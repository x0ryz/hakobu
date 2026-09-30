package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Under rootless Docker the containers' networks live in RootlessKit's
// network namespace, out of the host's reach. DialContainer reaches them
// through a helper: this binary, started with nsenter inside that
// namespace. For each connection hakobu sends the helper the address and one
// end of a fresh socketpair; the helper dials and sends the connected socket
// back over it (SCM_RIGHTS). A socket keeps working outside the namespace it
// was made in, so what hakobu gets is a plain TCP connection. Nothing is
// listening anywhere, so there's no socket file a container could be given.

// dialerEnv marks the helper process; see RunDialerIfChild.
const dialerEnv = "HAKOBU_DIALER"

// dialerTimeout bounds the helper's dial when the caller set no deadline.
const dialerTimeout = 10 * time.Second

// errHelperGone means the helper didn't answer at all (it died, or Docker
// restarted under it), as opposed to the target refusing the connection.
var errHelperGone = errors.New("container dialer not running")

// ContainerTransport is for HTTP to containers (the app proxies, RustFS):
// it dials through DialContainer and ignores HTTP_PROXY, which is meant for
// the internet, not for container networks.
var ContainerTransport = newContainerTransport()

func newContainerTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.DialContext = DialContainer
	return t
}

// DialContainer dials addr (a container's ip:port) directly, or through the
// helper under rootless Docker. Its signature is http.Transport's
// DialContext.
func DialContainer(ctx context.Context, network, addr string) (net.Conn, error) {
	rootless, err := Rootless(ctx)
	if err != nil {
		return nil, err
	}
	if !rootless {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	return helper.dial(ctx, network, addr)
}

var rootlessMode struct {
	sync.Mutex
	known, on bool
}

// Rootless asks the daemon whether it runs rootless; a DOCKER_HOST alone
// doesn't say, a rootful daemon can be reached through one too. The answer
// is remembered once the daemon has given one.
func Rootless(ctx context.Context) (bool, error) {
	rootlessMode.Lock()
	defer rootlessMode.Unlock()
	if rootlessMode.known {
		return rootlessMode.on, nil
	}
	body, status, err := dockerRequest(ctx, "GET", "/info", nil)
	if err != nil {
		return false, err
	}
	if status != http.StatusOK {
		return false, fmt.Errorf("docker info failed (%d): %s", status, body)
	}
	var info struct {
		SecurityOptions []string `json:"SecurityOptions"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return false, err
	}
	rootlessMode.known = true
	rootlessMode.on = slices.Contains(info.SecurityOptions, "name=rootless")
	return rootlessMode.on, nil
}

// rootlessNetns is the namespace containers live in: RootlessKit's child
// process's (dockerd itself stays in the host's). It changes when Docker
// restarts. RootlessKit keeps its state next to the daemon's socket, both in
// $XDG_RUNTIME_DIR.
func rootlessNetns() (pid, ns string, err error) {
	b, err := os.ReadFile(filepath.Join(filepath.Dir(dockerSocket()), "dockerd-rootless", "child_pid"))
	if err != nil {
		return "", "", fmt.Errorf("rootless docker's network namespace: %w", err)
	}
	pid = strings.TrimSpace(string(b))
	ns, err = os.Readlink("/proc/" + pid + "/ns/net")
	if err != nil {
		return "", "", fmt.Errorf("rootless docker's network namespace: %w", err)
	}
	return pid, ns, nil
}

var helper = &dialerHelper{}

type dialerHelper struct {
	mu      sync.Mutex
	ns      string // the namespace the running helper is in
	control *net.UnixConn
	cmd     *exec.Cmd
	// start runs a helper and returns the control socket to it; tests
	// replace it with an in-process one.
	start func(pid string) (*net.UnixConn, *exec.Cmd, error)
}

// dial retries once with a fresh helper when the helper itself failed, not
// when the target refused.
func (h *dialerHelper) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	for attempt := 0; ; attempt++ {
		control, err := h.current()
		if err != nil {
			return nil, err
		}
		conn, err := dialVia(ctx, control, network, addr)
		if !errors.Is(err, errHelperGone) || attempt == 1 {
			return conn, err
		}
		h.drop(control)
	}
}

// current returns the control socket of a helper in the current namespace,
// starting one if there's none yet or Docker restarted: an old helper
// doesn't die then, it keeps the old namespace alive and sees none of the
// new containers.
func (h *dialerHelper) current() (*net.UnixConn, error) {
	pid, ns, err := rootlessNetns()
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.control != nil && h.ns == ns {
		return h.control, nil
	}
	h.stopLocked()
	start := h.start
	if start == nil {
		start = startHelper
	}
	control, cmd, err := start(pid)
	if err != nil {
		return nil, err
	}
	h.ns, h.control, h.cmd = ns, control, cmd
	return control, nil
}

// drop stops the helper behind control unless another caller already
// replaced it.
func (h *dialerHelper) drop(control *net.UnixConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.control == control {
		h.stopLocked()
	}
}

func (h *dialerHelper) stopLocked() {
	if h.control != nil {
		_ = h.control.Close() // the helper exits when its control socket closes
	}
	if cmd := h.cmd; cmd != nil {
		_ = cmd.Process.Kill()
		go func() { _ = cmd.Wait() }()
	}
	h.ns, h.control, h.cmd = "", nil, nil
}

func startHelper(pid string) (*net.UnixConn, *exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, nil, err
	}
	ours, theirs, err := seqpacketPair()
	if err != nil {
		return nil, nil, err
	}
	defer theirs.Close()
	cmd := exec.Command("nsenter", "-U", "--preserve-credentials", "-n", "-t", pid, "--", exe)
	cmd.Env = append(os.Environ(), dialerEnv+"=1")
	cmd.ExtraFiles = []*os.File{theirs} // fd 3
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		ours.Close()
		return nil, nil, fmt.Errorf("start container dialer: %w", err)
	}
	control, err := unixConn(ours)
	if err != nil {
		_ = cmd.Process.Kill()
		go func() { _ = cmd.Wait() }()
		return nil, nil, err
	}
	return control, cmd, nil
}

// dialVia asks the helper behind control to dial addr.
func dialVia(ctx context.Context, control *net.UnixConn, network, addr string) (net.Conn, error) {
	ours, theirs, err := seqpacketPair()
	if err != nil {
		return nil, err
	}
	reply, err := unixConn(ours)
	if err != nil {
		theirs.Close()
		return nil, err
	}
	defer reply.Close()
	_, _, err = control.WriteMsgUnix([]byte(network+" "+addr), syscall.UnixRights(int(theirs.Fd())), nil)
	theirs.Close()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errHelperGone, err)
	}

	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(dialerTimeout + time.Second)
	}
	_ = reply.SetReadDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = reply.SetReadDeadline(time.Now()) })
	defer stop()
	buf, oob := make([]byte, 512), make([]byte, syscall.CmsgSpace(4))
	n, oobn, _, _, err := reply.ReadMsgUnix(buf, oob)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err) // our deadline passed
	}
	if n == 0 && oobn == 0 { // closed without an answer
		return nil, errHelperGone
	}
	fds, err := parseRights(oob[:oobn])
	if err != nil {
		return nil, err
	}
	if len(fds) == 0 {
		return nil, fmt.Errorf("dial %s: %s", addr, buf[:n])
	}
	f := os.NewFile(uintptr(fds[0]), "container-conn")
	defer f.Close() // FileConn dups it
	for _, fd := range fds[1:] {
		syscall.Close(fd)
	}
	return net.FileConn(f)
}

// RunDialerIfChild turns this process into the helper when hakobu started
// it as one; main and the TestMains of tests that deploy call it first.
func RunDialerIfChild() {
	if os.Getenv(dialerEnv) != "1" {
		return
	}
	control, err := unixConn(os.NewFile(3, "control"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "container dialer:", err)
		os.Exit(1)
	}
	serveDialer(control, allowedTarget)
	os.Exit(0)
}

// serveDialer answers dial requests until control closes.
func serveDialer(control *net.UnixConn, allowed func(ip net.IP) bool) {
	for {
		buf, oob := make([]byte, 512), make([]byte, syscall.CmsgSpace(4))
		n, oobn, _, _, err := control.ReadMsgUnix(buf, oob)
		if err != nil || (n == 0 && oobn == 0) {
			return // hakobu is gone
		}
		fds, err := parseRights(oob[:oobn])
		if err != nil || len(fds) != 1 {
			for _, fd := range fds {
				syscall.Close(fd)
			}
			continue
		}
		reply, err := unixConn(os.NewFile(uintptr(fds[0]), "reply"))
		if err != nil {
			continue
		}
		go func() {
			defer reply.Close()
			network, addr, _ := strings.Cut(string(buf[:n]), " ")
			conn, err := dialTarget(network, addr, allowed)
			if err != nil {
				_, _ = reply.Write([]byte(err.Error()))
				return
			}
			defer conn.Close()
			f, err := conn.File()
			if err != nil {
				_, _ = reply.Write([]byte(err.Error()))
				return
			}
			defer f.Close()
			_, _, _ = reply.WriteMsgUnix([]byte("ok"), syscall.UnixRights(int(f.Fd())), nil)
		}()
	}
}

func dialTarget(network, addr string, allowed func(ip net.IP) bool) (*net.TCPConn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("network %q not allowed", network)
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host)
	if ip == nil || !allowed(ip) {
		return nil, fmt.Errorf("%s is not on a container network", host)
	}
	conn, err := net.DialTimeout(network, addr, dialerTimeout)
	if err != nil {
		return nil, err
	}
	return conn.(*net.TCPConn), nil
}

// allowedTarget limits the helper to Docker's bridge networks: in
// RootlessKit's namespace the rest (its tap device, loopback) leads to the
// host and the internet, which hakobu never dials through here.
func allowedTarget(ip net.IP) bool {
	ifaces, err := net.Interfaces()
	if err != nil {
		return false
	}
	for _, iface := range ifaces {
		if iface.Name != "docker0" && !strings.HasPrefix(iface.Name, "br-") {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.Contains(ip) && !n.IP.Equal(ip) {
				return true
			}
		}
	}
	return false
}

func seqpacketPair() (ours, theirs *os.File, err error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	return os.NewFile(uintptr(fds[0]), "dialer"), os.NewFile(uintptr(fds[1]), "dialer"), nil
}

// unixConn wraps f, closing it: the conn holds its own copy.
func unixConn(f *os.File) (*net.UnixConn, error) {
	defer f.Close()
	c, err := net.FileConn(f)
	if err != nil {
		return nil, err
	}
	uc, ok := c.(*net.UnixConn)
	if !ok {
		c.Close()
		return nil, fmt.Errorf("fd %d is not a unix socket", f.Fd())
	}
	return uc, nil
}

func parseRights(oob []byte) ([]int, error) {
	msgs, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, err
	}
	var fds []int
	for _, m := range msgs {
		got, err := syscall.ParseUnixRights(&m)
		if err != nil {
			return nil, err
		}
		fds = append(fds, got...)
	}
	return fds, nil
}
