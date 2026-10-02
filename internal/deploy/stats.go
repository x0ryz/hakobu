package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// RunningContainer is a container Docker runs, by name without the slash.
type RunningContainer struct {
	ID   string
	Name string
	App  string // its AppLabel, "" if none
}

// RunningContainers lists the running containers.
func RunningContainers(ctx context.Context) ([]RunningContainer, error) {
	respBody, status, err := dockerRequest(ctx, "GET", "/containers/json", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("container list failed (%d): %s", status, respBody)
	}
	var list []struct {
		ID     string            `json:"Id"`
		Names  []string          `json:"Names"`
		Labels map[string]string `json:"Labels"`
	}
	if err := json.Unmarshal(respBody, &list); err != nil {
		return nil, err
	}
	out := make([]RunningContainer, 0, len(list))
	for _, c := range list {
		if len(c.Names) == 0 {
			continue
		}
		out = append(out, RunningContainer{ID: c.ID, Name: strings.TrimPrefix(c.Names[0], "/"), App: c.Labels[AppLabel]})
	}
	return out, nil
}

// Counters are a container's usage so far; CPU and I/O only mean
// something as the difference between two readings.
type Counters struct {
	CPUNanos  uint64 // CPU time used
	Memory    uint64 // in use, without the page cache it can drop
	MemLimit  uint64 // the host's memory when it has no limit
	NetRx     uint64
	NetTx     uint64
	DiskRead  uint64
	DiskWrite uint64
}

// ContainerStats reads a container's counters once, without the second
// that Docker otherwise waits to work out the CPU usage itself.
func ContainerStats(ctx context.Context, id string) (Counters, error) {
	respBody, status, err := dockerRequest(ctx, "GET", "/containers/"+url.PathEscape(id)+"/stats?stream=false&one-shot=true", nil)
	if err != nil {
		return Counters{}, err
	}
	if status != http.StatusOK {
		return Counters{}, fmt.Errorf("container stats failed (%d): %s", status, respBody)
	}
	return ParseStats(respBody)
}

// ParseStats reads the body of a stats request.
func ParseStats(b []byte) (Counters, error) {
	var st struct {
		CPU struct {
			Usage struct {
				Total uint64 `json:"total_usage"`
			} `json:"cpu_usage"`
		} `json:"cpu_stats"`
		Memory struct {
			Usage uint64            `json:"usage"`
			Limit uint64            `json:"limit"`
			Stats map[string]uint64 `json:"stats"`
		} `json:"memory_stats"`
		Networks map[string]struct {
			Rx uint64 `json:"rx_bytes"`
			Tx uint64 `json:"tx_bytes"`
		} `json:"networks"`
		Blkio struct {
			Bytes []struct {
				Op    string `json:"op"`
				Value uint64 `json:"value"`
			} `json:"io_service_bytes_recursive"`
		} `json:"blkio_stats"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return Counters{}, err
	}
	c := Counters{CPUNanos: st.CPU.Usage.Total, Memory: st.Memory.Usage, MemLimit: st.Memory.Limit}
	// Like `docker stats`: the page cache the kernel can take back isn't
	// the app's (cgroup v2 calls it inactive_file, v1 total_inactive_file).
	cache := st.Memory.Stats["inactive_file"]
	if v, ok := st.Memory.Stats["total_inactive_file"]; ok {
		cache = v
	}
	if cache < c.Memory {
		c.Memory -= cache
	}
	for _, n := range st.Networks {
		c.NetRx += n.Rx
		c.NetTx += n.Tx
	}
	for _, e := range st.Blkio.Bytes {
		switch strings.ToLower(e.Op) {
		case "read":
			c.DiskRead += e.Value
		case "write":
			c.DiskWrite += e.Value
		}
	}
	return c, nil
}
