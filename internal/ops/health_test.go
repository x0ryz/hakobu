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
