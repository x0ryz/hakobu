package ops

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/deploy"
	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store"
	"github.com/x0ryz/hakobu/internal/store/teldb"
)

// healthFailures is how many checks in a row an app fails before it counts
// as down: one slow answer during a garbage collection isn't an outage.
const healthFailures = 3

// failedChecks counts the failed checks in a row of each app; an app is
// down from healthFailures on.
var failedChecks struct {
	sync.Mutex
	n map[string]int
}

// WatchHealth checks every live app the way its deploy did, on the docker
// network, and records an app that stops answering in its Errors tab and
// emails the owner, and again when it answers again. A container that
// isn't running is left to WatchDeaths, which reports the crash.
func WatchHealth(s *store.Store) {
	for {
		time.Sleep(config.HealthCheckEvery)
		apps, err := s.ListApps(ctx())
		if err != nil {
			continue
		}
		var wg sync.WaitGroup
		for _, app := range apps {
			if app.LivePort == 0 || IsDeploying(app.Name) {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				if ok, why, checked := checkHealth(app); checked {
					noteHealth(s, app.Name, ok, why)
				}
			}()
		}
		wg.Wait()
		forgetHealth(apps)
	}
}

// checkHealth asks the live container of app for its health check path;
// checked is false when there was nothing to ask.
func checkHealth(app store.App) (ok bool, why string, checked bool) {
	container := app.ContainerName()
	if deploy.ContainerState(ctx(), container).Status != "running" {
		return false, "", false
	}
	ip, err := deploy.ContainerIP(ctx(), container, ProjectNetwork(app.ProjectName))
	if err != nil {
		return false, "", false
	}
	path := "/" + strings.TrimPrefix(app.HealthCheckPath, "/")
	requireOK := path != "/"
	if deploy.HTTPCheck(fmt.Sprintf("http://%s:%d%s", ip, app.LivePort, path), requireOK) {
		return true, "", true
	}
	if requireOK {
		return false, fmt.Sprintf("didn't return 2xx on port %d%s", app.LivePort, path), true
	}
	return false, fmt.Sprintf("didn't answer on port %d", app.LivePort), true
}

// noteHealth counts a check of app and records the change when the app
// goes down or comes back.
func noteHealth(s *store.Store, app string, ok bool, why string) {
	failedChecks.Lock()
	if failedChecks.n == nil {
		failedChecks.n = map[string]int{}
	}
	before := failedChecks.n[app]
	if ok {
		delete(failedChecks.n, app)
	} else {
		failedChecks.n[app]++
	}
	wasDown, isDown := before >= healthFailures, failedChecks.n[app] >= healthFailures
	failedChecks.Unlock()

	switch {
	case isDown && !wasDown:
		message := fmt.Sprintf("%s %s %d times in a row: it's running but not serving.", app, why, healthFailures)
		recordHealth(s, app, "error", message)
		problem(s, "health:"+app, notifyAgain, app+": not responding",
			message+"\n\nIts output: "+panelURL("/apps/"+app+"#output"))
	case wasDown && !isDown:
		recordHealth(s, app, "info", app+" answers its health check again.")
		solved(s, "health:"+app, app+": responding again", app+" answers its health check again.\n\n"+panelURL("/apps/"+app))
	}
}

func recordHealth(s *store.Store, app, level, message string) {
	if err := s.Tel.CreateTelemetryEvent(ctx(), teldb.CreateTelemetryEventParams{
		AppName: app, Kind: "health", Level: level, Message: secret.String(message),
	}); err != nil {
		fmt.Println("failed to record the health of", app+":", err)
	}
}

// forgetHealth drops the counts of deleted apps.
func forgetHealth(apps []store.App) {
	live := map[string]bool{}
	for _, a := range apps {
		live[a.Name] = true
	}
	failedChecks.Lock()
	defer failedChecks.Unlock()
	for name := range failedChecks.n {
		if !live[name] {
			delete(failedChecks.n, name)
		}
	}
}

// AppEvents lists an app's telemetry, newest first: with logs, or only
// what went wrong (errors, crashes, out-of-memory kills, health checks),
// which an app's logs would otherwise bury.
func AppEvents(s *store.Store, app string, logs bool, limit int64) ([]teldb.TelemetryEvent, error) {
	if logs {
		return s.Tel.ListTelemetryEvents(ctx(), teldb.ListTelemetryEventsParams{AppName: app, Limit: limit})
	}
	return s.Tel.ListProblems(ctx(), teldb.ListProblemsParams{AppName: app, Limit: limit})
}
