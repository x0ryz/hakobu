package cmd

import (
	"strings"
	"testing"

	"github.com/x0ryz/hakobu/internal/store/teldb"
)

func TestUsageChart(t *testing.T) {
	rows := []teldb.Sample{
		{Ts: 0, Mem: 100 << 20, MemMax: 150 << 20, MemLimit: 512 << 20},
		{Ts: 60, Mem: 200 << 20, MemMax: 200 << 20, MemLimit: 512 << 20},
		{Ts: 600, Mem: 300 << 20, MemMax: 400 << 20, MemLimit: 512 << 20}, // after a gap
	}
	charts := targetCharts("app:web", rows, 0, 600)
	if len(charts) != 3 {
		t.Fatalf("%d charts, want CPU, memory and network", len(charts))
	}
	mem := charts[1]
	if mem.Title != "App · memory" || mem.Now != "300 MB of 512 MB" || mem.Peak != "peak 400 MB" {
		t.Errorf("memory chart reads %q, %q, %q", mem.Title, mem.Now, mem.Peak)
	}
	svg := string(mem.SVG)
	if n := strings.Count(svg, "<polyline"); n != 2 {
		t.Errorf("%d lines, want the gap to split the series in 2", n)
	}
	if !strings.Contains(svg, `stroke-dasharray`) {
		t.Error("no line for the memory limit")
	}
	if got := usageRows(rows[:1])[0]; got.MemPct != 20 {
		t.Errorf("memory at %d%% of the limit, want 20", got.MemPct)
	}
}

func TestSummarizeUsageForMCP(t *testing.T) {
	var rows []teldb.Sample
	for i := range 120 {
		rows = append(rows, teldb.Sample{Ts: int64(i) * 60, Cpu: 0.5, CpuMax: 0.5, CpuLimit: 1, Mem: 100 << 20, MemMax: 100 << 20})
	}
	rows[7].CpuMax, rows[7].MemMax = 0.9, 300<<20
	u := summarizeUsage("app:web", "24h", rows)
	if len(u.Series) != 60 {
		t.Errorf("%d points, want 60", len(u.Series))
	}
	if u.CPU != (mcpStat{Now: 0.5, Avg: 0.5, Peak: 0.9, Limit: 1}) || u.Memory.Peak != 300 || u.Memory.Limit != 0 {
		t.Errorf("stats %+v %+v", u.CPU, u.Memory)
	}
	if u.Series[3].CPUPeak != 0.9 {
		t.Errorf("the peak got lost thinning the series: %+v", u.Series[3])
	}
	if empty := summarizeUsage("host", "1h", nil); empty.Series == nil {
		t.Error("no samples should still be an empty list, not null")
	}
}
