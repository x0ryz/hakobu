// Package deploy talks to the Docker Engine API over its unix socket.
package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// NetworkName is the home network of hakobu's services (Postgres, RustFS),
// EdgeNetwork cloudflared's. Apps live on their project's networks (see
// ops), which the services and cloudflared join as needed. Tests use
// networks of their own.
var (
	NetworkName = "hakobu"
	EdgeNetwork = "hakobu-edge"
)

// dockerSocket is the daemon's socket: DOCKER_HOST (unix:// only) when set,
// as under rootless Docker, else the rootful default.
func dockerSocket() string {
	if p, ok := strings.CutPrefix(os.Getenv("DOCKER_HOST"), "unix://"); ok && p != "" {
		return p
	}
	return "/var/run/docker.sock"
}

var dockerClient = &http.Client{
	Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", dockerSocket())
		},
	},
}

func dockerRequest(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reader = bytes.NewReader(b)
	}
	// The host is ignored: the transport always dials the unix socket.
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, reader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := dockerClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("docker daemon unreachable: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	return respBody, resp.StatusCode, err
}

// EnsureNetwork creates a bridge network unless it exists.
func EnsureNetwork(ctx context.Context, name string) error {
	return ensureNetwork(ctx, name)
}

func ensureNetwork(ctx context.Context, name string) error {
	if _, status, err := dockerRequest(ctx, "GET", "/networks/"+name, nil); err == nil && status == http.StatusOK {
		return nil
	}
	respBody, status, err := dockerRequest(ctx, "POST", "/networks/create", map[string]any{"Name": name, "Driver": "bridge"})
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return fmt.Errorf("network create failed (%d): %s", status, respBody)
	}
	return nil
}

// runContainer replaces any container with the same name and starts a new
// one on the hakobu network (or hostConfig's NetworkMode), with
// restart=unless-stopped unless hostConfig sets another restart policy.
func runContainer(ctx context.Context, name string, spec map[string]any, hostConfig map[string]any) (string, error) {
	if n, _ := hostConfig["NetworkMode"].(string); n == "" {
		hostConfig["NetworkMode"] = NetworkName
	}
	if err := ensureNetwork(ctx, hostConfig["NetworkMode"].(string)); err != nil {
		return "", fmt.Errorf("failed to ensure network: %w", err)
	}
	if err := RemoveContainer(ctx, name); err != nil {
		return "", fmt.Errorf("failed to remove old container: %w", err)
	}
	if hostConfig["RestartPolicy"] == nil {
		hostConfig["RestartPolicy"] = map[string]string{"Name": "unless-stopped"}
	}
	spec["HostConfig"] = hostConfig

	respBody, status, err := dockerRequest(ctx, "POST", "/containers/create?name="+url.QueryEscape(name), spec)
	if err != nil {
		return "", err
	}
	if status != http.StatusCreated {
		return "", fmt.Errorf("container create failed (%d): %s", status, respBody)
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(respBody, &created); err != nil {
		return "", err
	}
	respBody, status, err = dockerRequest(ctx, "POST", "/containers/"+created.ID+"/start", nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusNoContent {
		return "", fmt.Errorf("container start failed (%d): %s", status, respBody)
	}
	return created.ID, nil
}

// AppLabel names the app an app or worker container belongs to.
const AppLabel = "hakobu.app"

// AppOptions are what an app's containers (app and worker) have in common.
type AppOptions struct {
	App      string
	Network  string // the project's network
	Env      []string
	Binds    []string
	MemoryMB int64   // 0: no limit
	CPUs     float64 // 0: no limit
}

func (o AppOptions) spec(imageTag string) (spec, hostConfig map[string]any) {
	spec = map[string]any{"Image": imageTag, "Env": o.Env, "Labels": map[string]string{AppLabel: o.App}}
	hostConfig = map[string]any{
		"NetworkMode": o.Network,
		"Binds":       o.Binds,
		// setuid binaries can't raise privileges inside the container.
		"SecurityOpt": []string{"no-new-privileges"},
	}
	if o.MemoryMB > 0 {
		hostConfig["Memory"] = o.MemoryMB << 20
		hostConfig["MemorySwap"] = o.MemoryMB << 20 // no swap on top
	}
	if o.CPUs > 0 {
		hostConfig["NanoCpus"] = int64(o.CPUs * 1e9)
	}
	return spec, hostConfig
}

// RunAppContainer starts an app container with no host port published; the
// in-process proxy reaches it by its network IP. It isn't restarted until
// KeepRestarting: a candidate that crashes stays exited, with its exit
// reason, for the health check to see.
func RunAppContainer(ctx context.Context, imageTag, name string, opts AppOptions) (string, error) {
	spec, hostConfig := opts.spec(imageTag)
	hostConfig["RestartPolicy"] = map[string]string{"Name": "no"}
	return runContainer(ctx, name, spec, hostConfig)
}

// KeepRestarting has Docker restart the container whenever it stops, until
// it's stopped on purpose.
func KeepRestarting(ctx context.Context, name string) error {
	respBody, status, err := dockerRequest(ctx, "POST", "/containers/"+url.PathEscape(name)+"/update",
		map[string]any{"RestartPolicy": map[string]string{"Name": "unless-stopped"}})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("container update failed (%d): %s", status, respBody)
	}
	return nil
}

// RunWorkerContainer starts a worker from the app's image with a shell command.
func RunWorkerContainer(ctx context.Context, imageTag, name, command string, opts AppOptions) (string, error) {
	spec, hostConfig := opts.spec(imageTag)
	if command != "" {
		spec["Cmd"] = []string{"sh", "-c", command}
	}
	return runContainer(ctx, name, spec, hostConfig)
}

// RunServiceContainer starts a hakobu-managed backing service (Postgres,
// RustFS) with a persistent named volume.
func RunServiceContainer(ctx context.Context, name, image string, env []string, mountPath string) (string, error) {
	if err := pullImageIfMissing(ctx, image); err != nil {
		return "", fmt.Errorf("failed to pull image %q: %w", image, err)
	}
	return runContainer(ctx, name, map[string]any{"Image": image, "Env": env}, map[string]any{
		"Binds": []string{name + "_data:" + mountPath},
	})
}

// RunTunnelContainer runs cloudflared for the tunnel with the given token on
// the edge network. socketDir, holding the panel's unix socket, is mounted
// at /run/hakobu.
func RunTunnelContainer(ctx context.Context, name, image, token, socketDir string) (string, error) {
	if err := pullImageIfMissing(ctx, image); err != nil {
		return "", fmt.Errorf("failed to pull image %q: %w", image, err)
	}
	return runContainer(ctx, name, map[string]any{
		"Image": image,
		"Cmd":   []string{"tunnel", "run"},
		"Env":   []string{"TUNNEL_TOKEN=" + token},
	}, map[string]any{
		"NetworkMode": EdgeNetwork,
		"Binds":       []string{socketDir + ":/run/hakobu:z"},
		"SecurityOpt": []string{"no-new-privileges"},
	})
}

// ConnectNetwork adds a container to another network, with aliases if
// given; a no-op if it's on it already. Joining a network leaves the
// container's connections on its other networks alone.
func ConnectNetwork(ctx context.Context, container, network string, aliases ...string) error {
	info, err := inspect(ctx, container)
	if err != nil {
		return err
	}
	if info == nil {
		return fmt.Errorf("container %q not found", container)
	}
	if _, ok := info.NetworkSettings.Networks[network]; ok {
		return nil
	}
	if err := ensureNetwork(ctx, network); err != nil {
		return err
	}
	respBody, status, err := dockerRequest(ctx, "POST", "/networks/"+url.PathEscape(network)+"/connect", map[string]any{
		"Container": container, "EndpointConfig": map[string]any{"Aliases": aliases},
	})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("connecting %s to %s failed (%d): %s", container, network, status, respBody)
	}
	return nil
}

// DisconnectNetwork takes a container off a network; a no-op if it isn't
// on it or doesn't exist.
func DisconnectNetwork(ctx context.Context, container, network string) error {
	info, err := inspect(ctx, container)
	if err != nil || info == nil {
		return err
	}
	if _, ok := info.NetworkSettings.Networks[network]; !ok {
		return nil
	}
	respBody, status, err := dockerRequest(ctx, "POST", "/networks/"+url.PathEscape(network)+"/disconnect", map[string]any{"Container": container})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("disconnecting %s from %s failed (%d): %s", container, network, status, respBody)
	}
	return nil
}

// RemoveNetwork deletes a network; a missing one is fine.
func RemoveNetwork(ctx context.Context, name string) error {
	respBody, status, err := dockerRequest(ctx, "DELETE", "/networks/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusNoContent, http.StatusOK, http.StatusNotFound:
		return nil
	}
	return fmt.Errorf("removing network %s failed (%d): %s", name, status, respBody)
}

// StopContainer stops a container but keeps it, so it can be started again;
// a no-op if it doesn't exist or isn't running.
func StopContainer(ctx context.Context, name string) error {
	respBody, status, err := dockerRequest(ctx, "POST", "/containers/"+url.PathEscape(name)+"/stop?t=10", nil)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusNoContent, http.StatusNotModified, http.StatusNotFound:
		return nil
	}
	return fmt.Errorf("failed to stop container %s (%d): %s", name, status, respBody)
}

// RemoveVolume deletes a volume; in-use and missing volumes are left alone.
func RemoveVolume(ctx context.Context, name string) error {
	respBody, status, err := dockerRequest(ctx, "DELETE", "/volumes/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusNoContent, http.StatusNotFound, http.StatusConflict:
		return nil
	}
	return fmt.Errorf("failed to remove volume %s (%d): %s", name, status, respBody)
}

// VolumeNames lists volumes whose name starts with prefix.
func VolumeNames(ctx context.Context, prefix string) ([]string, error) {
	filters, _ := json.Marshal(map[string][]string{"name": {prefix}})
	respBody, status, err := dockerRequest(ctx, "GET", "/volumes?filters="+url.QueryEscape(string(filters)), nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("volume list failed (%d): %s", status, respBody)
	}
	var res struct {
		Volumes []struct{ Name string } `json:"Volumes"`
	}
	if err := json.Unmarshal(respBody, &res); err != nil {
		return nil, err
	}
	var names []string
	for _, v := range res.Volumes {
		if strings.HasPrefix(v.Name, prefix) { // the filter matches substrings
			names = append(names, v.Name)
		}
	}
	return names, nil
}

// RemoveContainer force-removes a container; a no-op if it doesn't exist.
func RemoveContainer(ctx context.Context, name string) error {
	respBody, status, err := dockerRequest(ctx, "DELETE", "/containers/"+url.PathEscape(name)+"?force=true", nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent && status != http.StatusNotFound {
		return fmt.Errorf("failed to remove container %s (%d): %s", name, status, respBody)
	}
	return nil
}

func pullImageIfMissing(ctx context.Context, image string) error {
	if ok, err := ImageExists(ctx, image); err != nil || ok {
		return err
	}
	respBody, status, err := dockerRequest(ctx, "POST", "/images/create?fromImage="+url.QueryEscape(image), nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("image pull failed (%d): %s", status, respBody)
	}
	return nil
}

// healthTransport keeps no connections: each check reaches a fresh candidate.
var healthTransport = func() *http.Transport {
	t := newContainerTransport()
	t.DisableKeepAlives = true
	return t
}()

// HTTPCheck GETs url. With requireOK, only a 2xx counts as healthy; without
// it any response does (the app may never have handled a bare "/").
func HTTPCheck(url string, requireOK bool) bool {
	client := &http.Client{
		Timeout: 3 * time.Second,
		// The app answers for itself; following its redirects would let it
		// point hakobu at anything reachable from the host.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     healthTransport,
	}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return !requireOK || (resp.StatusCode >= 200 && resp.StatusCode < 300)
}

func WaitHealthy(attempts int, interval time.Duration, check func() bool) bool {
	for i := 0; i < attempts; i++ {
		if check() {
			return true
		}
		time.Sleep(interval)
	}
	return false
}

// demux strips Docker's 8-byte stream frame headers from non-TTY output.
func demux(r io.Reader) string {
	var out bytes.Buffer
	header := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, header); err != nil {
			break
		}
		size := int64(header[4])<<24 | int64(header[5])<<16 | int64(header[6])<<8 | int64(header[7])
		if _, err := io.CopyN(&out, r, size); err != nil {
			break
		}
	}
	return out.String()
}

func execInContainer(ctx context.Context, containerName string, cmd []string) (output string, exitCode int, err error) {
	respBody, status, err := dockerRequest(ctx, "POST", "/containers/"+containerName+"/exec", map[string]any{
		"Cmd": cmd, "AttachStdout": true, "AttachStderr": true,
	})
	if err != nil {
		return "", 0, err
	}
	if status != http.StatusCreated {
		return "", 0, fmt.Errorf("exec create failed (%d): %s", status, respBody)
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(respBody, &created); err != nil {
		return "", 0, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "http://docker/exec/"+created.ID+"/start", strings.NewReader(`{"Detach":false,"Tty":false}`))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := dockerClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("docker daemon unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", 0, fmt.Errorf("exec start failed (%d): %s", resp.StatusCode, body)
	}
	output = demux(resp.Body)

	inspectBody, status, err := dockerRequest(ctx, "GET", "/exec/"+created.ID+"/json", nil)
	if err != nil {
		return output, 0, err
	}
	if status != http.StatusOK {
		return output, 0, fmt.Errorf("exec inspect failed (%d): %s", status, inspectBody)
	}
	var inspect struct {
		ExitCode int `json:"ExitCode"`
	}
	err = json.Unmarshal(inspectBody, &inspect)
	return output, inspect.ExitCode, err
}

// PostgresReady checks over TCP: the image's init-time server only listens
// on the unix socket, so this doesn't report ready before init finishes.
func PostgresReady(ctx context.Context, containerName string) bool {
	_, exitCode, err := execInContainer(ctx, containerName, []string{"pg_isready", "-h", "127.0.0.1", "-U", "postgres"})
	return err == nil && exitCode == 0
}

// PostgresExec runs one SQL statement as the postgres superuser (local trust auth).
func PostgresExec(ctx context.Context, containerName, sql string) error {
	out, exitCode, err := execInContainer(ctx, containerName, []string{"psql", "-U", "postgres", "-c", sql})
	if err != nil {
		return err
	}
	if exitCode != 0 {
		return fmt.Errorf("psql exited %d: %s", exitCode, strings.TrimSpace(out))
	}
	return nil
}

// ContainerLogs returns the last tailLines of stdout+stderr.
func ContainerLogs(ctx context.Context, containerName string, tailLines int) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://docker/containers/%s/logs?stdout=1&stderr=1&tail=%d&timestamps=1", containerName, tailLines), nil)
	if err != nil {
		return "", err
	}
	resp, err := dockerClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("docker daemon unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("logs failed (%d): %s", resp.StatusCode, body)
	}
	return demux(resp.Body), nil
}

type containerInfo struct {
	Config struct {
		Env []string `json:"Env"`
	} `json:"Config"`
	State struct {
		Status    string `json:"Status"`
		Pid       int    `json:"Pid"`
		OOMKilled bool   `json:"OOMKilled"`
		ExitCode  int    `json:"ExitCode"`
	} `json:"State"`
	RestartCount    int `json:"RestartCount"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

func inspect(ctx context.Context, containerName string) (*containerInfo, error) {
	respBody, status, err := dockerRequest(ctx, "GET", "/containers/"+containerName+"/json", nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, nil
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("container inspect failed (%d): %s", status, respBody)
	}
	var info containerInfo
	return &info, json.Unmarshal(respBody, &info)
}

// ContainerEnv returns the container's environment, or nil if it doesn't exist.
func ContainerEnv(ctx context.Context, containerName string) (map[string]string, error) {
	info, err := inspect(ctx, containerName)
	if err != nil || info == nil {
		return nil, err
	}
	env := map[string]string{}
	for _, kv := range info.Config.Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	return env, nil
}

// StartContainer starts an existing, stopped container.
func StartContainer(ctx context.Context, containerName string) error {
	respBody, status, err := dockerRequest(ctx, "POST", "/containers/"+containerName+"/start", nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent && status != http.StatusNotModified {
		return fmt.Errorf("container start failed (%d): %s", status, respBody)
	}
	return nil
}

// ListeningPorts returns the TCP ports the container listens on, split into
// ports reachable from other containers and ones bound to loopback only.
// It reads the container's /proc/<pid>/net/tcp{,6} from the host (hakobu runs
// as root), falling back to `cat` inside the container.
func ListeningPorts(ctx context.Context, containerName string) (reachable, loopback []int, err error) {
	info, err := inspect(ctx, containerName)
	if err != nil || info == nil || info.State.Pid == 0 {
		return nil, nil, err
	}
	var tables string
	for _, f := range []string{"tcp", "tcp6"} {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/net/%s", info.State.Pid, f))
		if err != nil {
			out, _, err := execInContainer(ctx, containerName, []string{"cat", "/proc/net/tcp", "/proc/net/tcp6"})
			if err != nil {
				return nil, nil, err
			}
			tables = out
			break
		}
		tables += string(b)
	}
	seen := map[int]bool{}
	for _, line := range strings.Split(tables, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[3] != "0A" { // 0A = LISTEN
			continue
		}
		addr, portHex, ok := strings.Cut(fields[1], ":")
		if !ok {
			continue
		}
		port64, err := strconv.ParseInt(portHex, 16, 32)
		if err != nil || seen[int(port64)] {
			continue
		}
		port := int(port64)
		seen[port] = true
		// 127.0.0.1 in tcp, ::1 in tcp6 (little-endian hex).
		if addr == "0100007F" || addr == "00000000000000000000000001000000" || addr == "0000000000000000FFFF00000100007F" {
			loopback = append(loopback, port)
		} else {
			reachable = append(reachable, port)
		}
	}
	sort.Ints(reachable)
	return reachable, loopback, nil
}

// ExposedPort returns the first port an image EXPOSEs, 0 if none.
func ExposedPort(ctx context.Context, image string) int {
	respBody, status, err := dockerRequest(ctx, "GET", "/images/"+url.PathEscape(image)+"/json", nil)
	if err != nil || status != http.StatusOK {
		return 0
	}
	var img struct {
		Config struct {
			ExposedPorts map[string]struct{} `json:"ExposedPorts"`
		} `json:"Config"`
	}
	json.Unmarshal(respBody, &img)
	ports := []int{}
	for p := range img.Config.ExposedPorts {
		if n, err := strconv.Atoi(strings.TrimSuffix(p, "/tcp")); err == nil {
			ports = append(ports, n)
		}
	}
	sort.Ints(ports)
	if len(ports) == 0 {
		return 0
	}
	return ports[0]
}

// ContainerIP returns the container's address on network.
func ContainerIP(ctx context.Context, containerName, network string) (string, error) {
	info, err := inspect(ctx, containerName)
	if err != nil {
		return "", err
	}
	if info == nil {
		return "", fmt.Errorf("container %q not found", containerName)
	}
	n, ok := info.NetworkSettings.Networks[network]
	if !ok || n.IPAddress == "" {
		return "", fmt.Errorf("container %q has no IP on network %q", containerName, network)
	}
	return n.IPAddress, nil
}

// State is what the panel shows about a container.
type State struct {
	Status    string // docker's state ("running", "exited", ...), "not found" or "unknown"
	Restarts  int    // surfaces crash loops
	OOMKilled bool   // the last exit was the kernel killing it for memory
}

func ContainerState(ctx context.Context, containerName string) State {
	info, err := inspect(ctx, containerName)
	if err != nil {
		return State{Status: "unknown"}
	}
	if info == nil {
		return State{Status: "not found"}
	}
	return State{Status: info.State.Status, Restarts: info.RestartCount, OOMKilled: info.State.OOMKilled}
}

// ContainerStatus is ContainerState's status and restart count.
func ContainerStatus(ctx context.Context, containerName string) (status string, restarts int) {
	st := ContainerState(ctx, containerName)
	return st.Status, st.Restarts
}

// WatchOOM calls fn for every container the kernel kills for running out of
// memory, until ctx ends or the event stream breaks. app is the container's
// AppLabel, "" for services such as Postgres.
func WatchOOM(ctx context.Context, fn func(container, app string)) error {
	filters, _ := json.Marshal(map[string][]string{"type": {"container"}, "event": {"oom"}})
	req, err := http.NewRequestWithContext(ctx, "GET", "http://docker/events?filters="+url.QueryEscape(string(filters)), nil)
	if err != nil {
		return err
	}
	resp, err := dockerClient.Do(req)
	if err != nil {
		return fmt.Errorf("docker daemon unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("events failed (%d): %s", resp.StatusCode, body)
	}
	dec := json.NewDecoder(resp.Body)
	for {
		var ev struct {
			Actor struct {
				Attributes map[string]string `json:"Attributes"`
			} `json:"Actor"`
		}
		if err := dec.Decode(&ev); err != nil {
			return err
		}
		fn(ev.Actor.Attributes["name"], ev.Actor.Attributes[AppLabel])
	}
}

// TagImage points targetRef ("repo:tag") at sourceRef's image.
func TagImage(ctx context.Context, sourceRef, targetRef string) error {
	repo, tag := targetRef, "latest"
	if i := strings.LastIndex(targetRef, ":"); i != -1 {
		repo, tag = targetRef[:i], targetRef[i+1:]
	}
	respBody, status, err := dockerRequest(ctx, "POST", "/images/"+url.PathEscape(sourceRef)+"/tag?repo="+url.QueryEscape(repo)+"&tag="+url.QueryEscape(tag), nil)
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return fmt.Errorf("image tag failed (%d): %s", status, respBody)
	}
	return nil
}

func ImageExists(ctx context.Context, ref string) (bool, error) {
	_, status, err := dockerRequest(ctx, "GET", "/images/"+url.PathEscape(ref)+"/json", nil)
	if err != nil {
		return false, err
	}
	switch status {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("image inspect failed (%d)", status)
	}
}

// ImageID returns the ID ref points at, "" if there's no such image.
func ImageID(ctx context.Context, ref string) string {
	respBody, status, err := dockerRequest(ctx, "GET", "/images/"+url.PathEscape(ref)+"/json", nil)
	if err != nil || status != http.StatusOK {
		return ""
	}
	var img struct {
		ID string `json:"Id"`
	}
	json.Unmarshal(respBody, &img)
	return img.ID
}

// RemoveImage removes a tag, or an image by ID, and the image itself once
// nothing else references it. Missing images and images still used by a
// container are left alone without an error.
func RemoveImage(ctx context.Context, ref string) error {
	respBody, status, err := dockerRequest(ctx, "DELETE", "/images/"+url.PathEscape(ref), nil)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK, http.StatusNotFound, http.StatusConflict:
		return nil
	}
	return fmt.Errorf("image remove %s failed (%d): %s", ref, status, respBody)
}

// ImageTags lists the tags of images whose repository matches pattern,
// e.g. "hakobu/*".
func ImageTags(ctx context.Context, pattern string) ([]string, error) {
	filters, _ := json.Marshal(map[string][]string{"reference": {pattern}})
	respBody, status, err := dockerRequest(ctx, "GET", "/images/json?filters="+url.QueryEscape(string(filters)), nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("image list failed (%d): %s", status, respBody)
	}
	var images []struct {
		RepoTags []string `json:"RepoTags"`
	}
	if err := json.Unmarshal(respBody, &images); err != nil {
		return nil, err
	}
	var tags []string
	for _, img := range images {
		tags = append(tags, img.RepoTags...)
	}
	return tags, nil
}

// PruneBuildCache drops build cache unused for longer than keep, both
// Docker's (Dockerfile builds) and the buildkit container's (Railpack). It
// returns the bytes Docker reports as freed; buildkit doesn't report it.
func PruneBuildCache(ctx context.Context, keep time.Duration) (freed int64, err error) {
	filters, _ := json.Marshal(map[string][]string{"until": {keep.String()}})
	respBody, status, err := dockerRequest(ctx, "POST", "/build/prune?filters="+url.QueryEscape(string(filters)), nil)
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("build cache prune failed (%d): %s", status, respBody)
	}
	var res struct {
		SpaceReclaimed int64 `json:"SpaceReclaimed"`
	}
	json.Unmarshal(respBody, &res)

	if info, _ := inspect(ctx, "buildkit"); info != nil && info.State.Status == "running" {
		out, code, err := execInContainer(ctx, "buildkit", []string{"buildctl", "prune", "--keep-duration", keep.String()})
		if err == nil && code != 0 {
			err = fmt.Errorf("buildctl prune exited %d: %s", code, strings.TrimSpace(out))
		}
		if err != nil {
			return res.SpaceReclaimed, err
		}
	}
	return res.SpaceReclaimed, nil
}

// Disk reports usage of the filesystem Docker stores images and volumes on.
func Disk(ctx context.Context) (used, total uint64, err error) {
	respBody, status, err := dockerRequest(ctx, "GET", "/info", nil)
	if err != nil {
		return 0, 0, err
	}
	if status != http.StatusOK {
		return 0, 0, fmt.Errorf("docker info failed (%d)", status)
	}
	var info struct {
		DockerRootDir string `json:"DockerRootDir"`
	}
	if err := json.Unmarshal(respBody, &info); err != nil {
		return 0, 0, err
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(info.DockerRootDir, &st); err != nil {
		return 0, 0, err
	}
	total = st.Blocks * uint64(st.Bsize)
	return total - st.Bavail*uint64(st.Bsize), total, nil
}
