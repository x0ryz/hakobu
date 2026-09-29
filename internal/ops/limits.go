package ops

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/x0ryz/hakobu/internal/deploy"
	"github.com/x0ryz/hakobu/internal/store"
)

// SetLimits caps the memory and CPUs of the app and its worker from the
// next deploy on; 0 removes a limit.
func SetLimits(s *store.Store, app string, memoryMB int64, cpus float64) error {
	if memoryMB != 0 && memoryMB < 16 {
		return fmt.Errorf("memory limit must be at least 16 MB, or empty for no limit")
	}
	if n := runtime.NumCPU(); cpus < 0 || cpus > float64(n) {
		return fmt.Errorf("CPU limit must be between 0.01 and %d (the server's CPUs), or empty for no limit", n)
	}
	if cpus != 0 && cpus < 0.01 {
		return fmt.Errorf("CPU limit must be at least 0.01")
	}
	return s.SetAppLimits(ctx(), store.SetAppLimitsParams{Name: app, MemoryMB: memoryMB, Cpus: cpus})
}

func oomText(app store.App) string {
	if app.MemoryMB > 0 {
		return fmt.Sprintf("it used more than its %d MB memory limit", app.MemoryMB)
	}
	return "the server ran out of memory"
}

// WatchOOM records every out-of-memory kill of an app or worker container
// in the app's Errors tab, reconnecting to Docker whenever the stream breaks.
func WatchOOM(s *store.Store) {
	for {
		err := deploy.WatchOOM(context.Background(), func(container, appName string) {
			if appName == "" { // containers from before hakobu labeled them
				appName = container
				for _, suffix := range []string{"-blue", "-green", "-worker"} {
					appName = strings.TrimSuffix(appName, suffix)
				}
			}
			app, err := s.GetApp(ctx(), appName)
			if err != nil {
				fmt.Println(container, "was killed: out of memory")
				return
			}
			s.CreateTelemetryEvent(ctx(), store.CreateTelemetryEventParams{
				AppName: app.Name, Kind: "oom", Level: "fatal",
				Message: fmt.Sprintf("%s was killed: %s. Docker restarts it.", container, oomText(app)),
			})
		})
		fmt.Println("docker events:", err)
		time.Sleep(5 * time.Second)
	}
}

// LastOOM is when the app or its worker last ran out of memory, "" if not
// within the retention period.
func LastOOM(s *store.Store, app string) string {
	at, _ := s.LastTelemetryOfKind(ctx(), store.LastTelemetryOfKindParams{AppName: app, Kind: "oom"})
	return at
}
