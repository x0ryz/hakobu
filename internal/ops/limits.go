package ops

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"github.com/x0ryz/hakobu/internal/deploy"
	"github.com/x0ryz/hakobu/internal/secret"
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

// WatchDeaths records every out-of-memory kill of an app or worker
// container, and every crash of a live one, in the app's Errors tab and
// emails the owner, reconnecting to Docker whenever the stream breaks.
func WatchDeaths(s *store.Store) {
	for {
		err := deploy.WatchDeaths(context.Background(), func(container, app string, d deploy.Death) { recordDeath(s, container, app, d) })
		fmt.Println("docker events:", err)
		time.Sleep(5 * time.Second)
	}
}

// recordDeath puts an out-of-memory kill of an app or worker container, or
// a crash of a live one, in the app's Errors tab and emails the owner.
func recordDeath(s *store.Store, container, appName string, d deploy.Death) {
	app, err := s.GetApp(ctx(), appName)
	if err != nil {
		if d.OOM {
			fmt.Println(container, "was killed, most likely out of memory")
		}
		return
	}
	var kind, message string
	switch {
	case d.OOM && d.Sure:
		kind, message = "oom", fmt.Sprintf("%s was killed: %s. Docker restarts it.", container, oomText(app))
	case d.OOM:
		kind, message = "oom", fmt.Sprintf("%s was killed (exit 137), most likely because %s. Docker restarts it.", container, oomText(app))
	case container == app.Name+"-"+app.ActiveSlot || container == app.Name+"-worker":
		// A candidate that crashes while starting fails its deploy,
		// which says so itself.
		kind, message = "crash", fmt.Sprintf("%s exited with code %s. Docker restarts it; its output says why.", container, d.ExitCode)
	default:
		return
	}
	if err := s.CreateTelemetryEvent(ctx(), store.CreateTelemetryEventParams{
		AppName: app.Name, Kind: kind, Level: "fatal", Message: secret.String(message),
	}); err != nil {
		fmt.Println("failed to record that", container, "stopped:", err)
	}
	if kind == "oom" {
		noteOOM(s, app.Name, message)
	} else {
		noteCrash(s, app.Name, message)
	}
}

// LastOOM is when the app or its worker last ran out of memory, "" if not
// within the retention period.
func LastOOM(s *store.Store, app string) string {
	at, _ := s.LastTelemetryOfKind(ctx(), store.LastTelemetryOfKindParams{AppName: app, Kind: "oom"})
	return at
}
