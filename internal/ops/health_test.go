package ops

import (
	"strings"
	"testing"

	"github.com/x0ryz/hakobu/internal/store/teldb"
)

func TestHealthOutages(t *testing.T) {
	f := newFakeEmail(t)
	s := notifyStore(t)
	if err := SetupNotifications(s, "me@example.org", "", ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { forgetHealth(nil) })
	check := func(ok bool) { noteHealth(s, "web", ok, "didn't answer on port 3000") }

	check(false)
	check(false)
	check(true) // two slow answers aren't an outage
	for range healthFailures + 2 {
		check(false)
	}
	check(true)
	check(true)

	if got, want := f.sent(), []string{"web: not responding", "web: responding again"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("sent %q, want %q", got, want)
	}
	events, err := s.Tel.ListTelemetryEvents(ctx(), teldb.ListTelemetryEventsParams{AppName: "web", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var levels []string
	for _, e := range events {
		if e.Kind != "health" {
			t.Errorf("recorded a %q", e.Kind)
		}
		levels = append(levels, e.Level)
	}
	if got := strings.Join(levels, ","); got != "info,error" {
		t.Errorf("recorded %s, want the outage and its end", got)
	}
}

// The Errors tab leaves the app's logs out unless asked: they'd bury the
// problems.
func TestAppEventsWithoutLogs(t *testing.T) {
	s := notifyStore(t)
	for _, kind := range []string{"error", "log", "log", "crash", "log"} {
		if err := s.Tel.CreateTelemetryEvent(ctx(), teldb.CreateTelemetryEventParams{AppName: "web", Kind: kind}); err != nil {
			t.Fatal(err)
		}
	}
	problems, err := AppEvents(s, "web", false, 10)
	if err != nil {
		t.Fatal(err)
	}
	all, _ := AppEvents(s, "web", true, 10)
	if len(problems) != 2 || problems[0].Kind != "crash" || len(all) != 5 {
		t.Errorf("problems %v, all %d", problems, len(all))
	}
}
