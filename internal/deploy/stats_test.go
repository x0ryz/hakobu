package deploy

import "testing"

func TestParseDockerStats(t *testing.T) {
	c, err := ParseStats([]byte(`{
		"cpu_stats": {"cpu_usage": {"total_usage": 5000}},
		"memory_stats": {"usage": 1000, "limit": 4096, "stats": {"inactive_file": 300}},
		"networks": {"eth0": {"rx_bytes": 10, "tx_bytes": 20}, "eth1": {"rx_bytes": 1, "tx_bytes": 2}},
		"blkio_stats": {"io_service_bytes_recursive": [{"op": "read", "value": 7}, {"op": "write", "value": 9}]}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	want := Counters{CPUNanos: 5000, Memory: 700, MemLimit: 4096, NetRx: 11, NetTx: 22, DiskRead: 7, DiskWrite: 9}
	if c != want {
		t.Errorf("got %+v, want %+v", c, want)
	}
}
