package ops

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/x0ryz/hakobu/internal/deploy"
	"github.com/x0ryz/hakobu/internal/store"
	"github.com/x0ryz/hakobu/internal/store/teldb"
)

// Every minute hakobu records the CPU, memory, network and disk use of the
// server, of each app and worker and of its own services in telemetry.db:
// a minute for 48 hours, then five-minute averages and peaks for a month,
// in rings of fixed slots (migration 002_samples.sql).
const (
	minuteRes   = 60
	fiveMinRes  = 5 * 60
	minuteSlots = 48 * 60          // 48 hours
	fiveSlots   = 30 * 24 * 60 / 5 // 30 days
)

// MetricRanges are the spans the panel and MCP show usage over.
var MetricRanges = map[string]time.Duration{
	"1h":  time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
}

// HostTarget is the server's own usage.
const HostTarget = "host"

// reading is a container's counters and when they were read.
type reading struct {
	c  deploy.Counters
	at time.Time
}

// metricState is what the next minute's usage is worked out from.
type metricState struct {
	containers map[string]reading // by container ID
	cpu        cpuTimes
	rolled     int64 // the last five minutes summarized
}

// WatchMetrics records usage every minute, on the minute.
func WatchMetrics(s *store.Store) {
	st := &metricState{containers: map[string]reading{}}
	for {
		next := time.Now().Truncate(time.Minute).Add(time.Minute)
		time.Sleep(time.Until(next))
		recordUsage(s, st, next)
	}
}

func recordUsage(s *store.Store, st *metricState, now time.Time) {
	ts := now.Unix()
	for _, u := range collectUsage(s, st, now) {
		if err := putSample(s, minuteRes, ts, u); err != nil {
			fmt.Println("failed to record the usage of", u.Target+":", err)
		}
	}
	// Summarize the five minutes before the current ones, once.
	if bucket := ts/fiveMinRes*fiveMinRes - fiveMinRes; bucket > st.rolled {
		if err := summarize(s, bucket); err != nil {
			fmt.Println("failed to summarize usage:", err)
		} else {
			st.rolled = bucket
		}
	}
	checkUsage(s, ts)
}

// putSample writes u into its slot of the ring of resolution res.
func putSample(s *store.Store, res, ts int64, u teldb.Sample) error {
	slots := int64(minuteSlots)
	if res == fiveMinRes {
		slots = fiveSlots
	}
	u.Res, u.Slot, u.Ts = res, ts/res%slots, ts
	return s.Tel.PutSample(ctx(), teldb.PutSampleParams(u))
}

// summarize writes the averages and peaks of the minutes in [bucket,
// bucket+5m) as one five-minute sample per target.
func summarize(s *store.Store, bucket int64) error {
	rows, err := s.Tel.SummarizeSamples(ctx(), teldb.SummarizeSamplesParams{Res: minuteRes, Ts: bucket, Ts_2: bucket + fiveMinRes})
	if err != nil {
		return err
	}
	for _, r := range rows {
		u := teldb.Sample{
			Target: r.Target, Cpu: r.Cpu, CpuMax: r.CpuMax, CpuLimit: r.CpuLimit,
			Mem: r.Mem, MemMax: r.MemMax, MemLimit: r.MemLimit,
			NetRx: r.NetRx, NetTx: r.NetTx, DiskRead: r.DiskRead, DiskWrite: r.DiskWrite,
			Load: r.Load, DiskUsed: r.DiskUsed, DiskTotal: r.DiskTotal,
		}
		if err := putSample(s, fiveMinRes, bucket, u); err != nil {
			return err
		}
	}
	return nil
}

// containerTarget is what a container's usage is recorded as.
type containerTarget struct {
	name     string
	cpuLimit float64
	memLimit int64
}

// containerTargets maps the containers worth recording by name: the live
// slot of each app (not the candidate of a deploy), its worker, and
// hakobu's services. Targets don't change when a deploy swaps slots.
func containerTargets(s *store.Store) map[string]containerTarget {
	targets := map[string]containerTarget{
		PostgresContainer: {name: "service:postgres"},
		tunnelContainer:   {name: "service:cloudflared"},
		"buildkit":        {name: "service:buildkit"},
	}
	apps, err := s.ListApps(ctx())
	if err != nil {
		return targets
	}
	for _, a := range apps {
		limit := containerTarget{cpuLimit: a.Cpus, memLimit: a.MemoryMB << 20}
		limit.name = "app:" + a.Name
		targets[a.ContainerName()] = limit
		limit.name = "worker:" + a.Name
		targets[a.Name+"-worker"] = limit
	}
	return targets
}

// collectUsage reads the usage of the last minute. A container shows up
// from its second reading on, and not after it restarted, which resets
// its counters.
func collectUsage(s *store.Store, st *metricState, now time.Time) []teldb.Sample {
	var out []teldb.Sample
	if u, ok := hostUsage(st); ok {
		out = append(out, u)
	}
	running, err := deploy.RunningContainers(ctx())
	if err != nil {
		fmt.Println("usage:", err)
		return out
	}
	targets := containerTargets(s)
	seen := map[string]bool{}
	for _, c := range running {
		t, ok := targets[c.Name]
		if !ok {
			continue
		}
		counters, err := deploy.ContainerStats(ctx(), c.ID)
		if err != nil {
			continue
		}
		seen[c.ID] = true
		prev, had := st.containers[c.ID]
		st.containers[c.ID] = reading{counters, now}
		if !had {
			continue
		}
		if u, ok := containerUsage(t, prev, reading{counters, now}); ok {
			out = append(out, u)
		}
	}
	for id := range st.containers {
		if !seen[id] {
			delete(st.containers, id)
		}
	}
	return out
}

// containerUsage is the usage between two readings of a container.
func containerUsage(t containerTarget, prev, cur reading) (teldb.Sample, bool) {
	secs := cur.at.Sub(prev.at).Seconds()
	p, c := prev.c, cur.c
	if secs <= 0 || c.CPUNanos < p.CPUNanos || c.NetRx < p.NetRx || c.NetTx < p.NetTx || c.DiskRead < p.DiskRead || c.DiskWrite < p.DiskWrite {
		return teldb.Sample{}, false // restarted
	}
	rate := func(a, b uint64) float64 { return float64(b-a) / secs }
	cpu := float64(c.CPUNanos-p.CPUNanos) / 1e9 / secs
	mem := int64(c.Memory)
	return teldb.Sample{
		Target: t.name, Cpu: cpu, CpuMax: cpu, CpuLimit: t.cpuLimit,
		Mem: mem, MemMax: mem, MemLimit: t.memLimit,
		NetRx: rate(p.NetRx, c.NetRx), NetTx: rate(p.NetTx, c.NetTx),
		DiskRead: rate(p.DiskRead, c.DiskRead), DiskWrite: rate(p.DiskWrite, c.DiskWrite),
	}, true
}

// procRoot is where the kernel's /proc is; tests point it at fixtures.
var procRoot = "/proc"

// cpuTimes are the server's CPU time so far, in clock ticks.
type cpuTimes struct {
	busy, total uint64
	cpus        int
}

// hostUsage is the server's usage since the last call; its CPU shows from
// the second call on.
func hostUsage(st *metricState) (teldb.Sample, bool) {
	cur, err := readCPUTimes()
	if err != nil {
		return teldb.Sample{}, false
	}
	prev := st.cpu
	st.cpu = cur
	if prev.total == 0 || cur.total <= prev.total || cur.busy < prev.busy {
		return teldb.Sample{}, false
	}
	u := teldb.Sample{Target: HostTarget, CpuLimit: float64(cur.cpus)}
	u.Cpu = float64(cur.busy-prev.busy) / float64(cur.total-prev.total) * float64(cur.cpus)
	u.CpuMax = u.Cpu
	if total, available, err := readMemInfo(); err == nil {
		u.Mem, u.MemLimit = total-available, total
		u.MemMax = u.Mem
	}
	u.Load, _ = readLoad()
	if used, total, err := deploy.Disk(ctx()); err == nil {
		u.DiskUsed, u.DiskTotal = int64(used), int64(total)
	}
	return u, true
}

// readCPUTimes reads the first line of /proc/stat; idle and iowait are
// the time not busy.
func readCPUTimes() (cpuTimes, error) {
	f, err := os.Open(filepath.Join(procRoot, "stat"))
	if err != nil {
		return cpuTimes{}, err
	}
	defer f.Close()
	var t cpuTimes
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "cpu") {
			continue
		}
		if fields[0] != "cpu" {
			t.cpus++
			continue
		}
		for i, v := range fields[1:] {
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				return cpuTimes{}, err
			}
			if i >= 8 { // guest time is already in user and nice
				break
			}
			t.total += n
			if i != 3 && i != 4 { // idle, iowait
				t.busy += n
			}
		}
	}
	if t.total == 0 || t.cpus == 0 {
		return cpuTimes{}, fmt.Errorf("no CPU times in %s/stat", procRoot)
	}
	return t, sc.Err()
}

// readMemInfo reads the server's memory and how much of it is available,
// in bytes.
func readMemInfo() (total, available int64, err error) {
	b, err := os.ReadFile(filepath.Join(procRoot, "meminfo"))
	if err != nil {
		return 0, 0, err
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total = kb << 10
		case "MemAvailable:":
			available = kb << 10
		}
	}
	if total == 0 {
		return 0, 0, fmt.Errorf("no MemTotal in %s/meminfo", procRoot)
	}
	return total, available, nil
}

// readLoad reads the one-minute load average.
func readLoad() (float64, error) {
	b, err := os.ReadFile(filepath.Join(procRoot, "loadavg"))
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0, fmt.Errorf("empty %s/loadavg", procRoot)
	}
	return strconv.ParseFloat(fields[0], 64)
}

// Usage alerts: the last minutes, all of them over the threshold.
const (
	usageThreshold = 0.9
	memoryMinutes  = 10
	cpuMinutes     = 15
)

// checkUsage mails the owner about an app or worker near its memory limit,
// and a server short of memory or CPU, for minutes on end.
func checkUsage(s *store.Store, ts int64) {
	memHigh := func(r teldb.Sample) bool {
		return r.MemLimit > 0 && float64(r.Mem) >= usageThreshold*float64(r.MemLimit)
	}
	cpuHigh := func(r teldb.Sample) bool {
		return r.CpuLimit > 0 && r.Cpu >= usageThreshold*r.CpuLimit
	}
	if apps, err := s.ListApps(ctx()); err == nil {
		for _, a := range apps {
			if a.MemoryMB == 0 {
				continue
			}
			for _, target := range []string{"app:" + a.Name, "worker:" + a.Name} {
				what := a.Name
				if strings.HasPrefix(target, "worker:") {
					what += "'s worker"
				}
				alertOn(s, target, ts, memoryMinutes, memHigh, "memory:"+target,
					what+": close to its memory limit",
					fmt.Sprintf("%s has used over %d%% of its %d MB memory limit for %d minutes; at the limit it's killed. Raise the limit or find what grows: %s",
						what, int(usageThreshold*100), a.MemoryMB, memoryMinutes, panelURL("/apps/"+a.Name)),
					what+": memory back under its limit", what+" uses less than "+strconv.Itoa(int(usageThreshold*100))+"% of its memory limit again.")
			}
		}
	}
	alertOn(s, HostTarget, ts, memoryMinutes, memHigh, "host-memory",
		"The server is short of memory",
		fmt.Sprintf("Over %d%% of the server's memory has been in use for %d minutes; apps and databases get killed when it runs out. See what uses it: %s",
			int(usageThreshold*100), memoryMinutes, panelURL("/settings#server")),
		"The server has memory to spare again", "Memory use of the server is back under "+strconv.Itoa(int(usageThreshold*100))+"%.")
	alertOn(s, HostTarget, ts, cpuMinutes, cpuHigh, "host-cpu",
		"The server's CPU is maxed out",
		fmt.Sprintf("The server's CPUs have been over %d%% busy for %d minutes; apps answer slowly. See what uses them: %s",
			int(usageThreshold*100), cpuMinutes, panelURL("/settings#server")),
		"The server's CPU has room again", "CPU use of the server is back under "+strconv.Itoa(int(usageThreshold*100))+"%.")
}

// alertOn mails problem when every minute of the last ones of target is
// high, and solved once the latest isn't.
func alertOn(s *store.Store, target string, ts int64, minutes int, high func(teldb.Sample) bool, key, subject, text, okSubject, okText string) {
	rows, err := s.Tel.ListSamples(ctx(), teldb.ListSamplesParams{Target: target, Res: minuteRes, Ts: ts - int64(minutes)*60 + 1})
	if err != nil || len(rows) == 0 {
		return
	}
	all := len(rows) >= minutes-1 // a minute may be missed
	for _, r := range rows {
		all = all && high(r)
	}
	switch {
	case all:
		problem(s, key, notifyAgain, subject, text)
	case !high(rows[len(rows)-1]):
		solved(s, key, okSubject, okText)
	}
}

// UsageOf reads the usage of target over the last span: by the minute up
// to 48 hours, by five minutes beyond.
func UsageOf(s *store.Store, target string, span time.Duration) ([]teldb.Sample, error) {
	res := int64(minuteRes)
	if span > minuteSlots*time.Minute {
		res = fiveMinRes
	}
	return s.Tel.ListSamples(ctx(), teldb.ListSamplesParams{Target: target, Res: res, Ts: time.Now().Add(-span).Unix()})
}

// CurrentUsage is the latest usage of every target, the server first,
// then by name.
func CurrentUsage(s *store.Store) ([]teldb.Sample, error) {
	rows, err := s.Tel.LatestSamples(ctx(), teldb.LatestSamplesParams{Res: minuteRes, Ts: time.Now().Add(-3 * time.Minute).Unix()})
	if err != nil {
		return nil, err
	}
	latest := map[string]teldb.Sample{}
	for _, r := range rows { // oldest first
		latest[r.Target] = r
	}
	out := make([]teldb.Sample, 0, len(latest))
	for _, r := range latest {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Target == HostTarget) != (out[j].Target == HostTarget) {
			return out[i].Target == HostTarget
		}
		return out[i].Target < out[j].Target
	})
	return out, nil
}
