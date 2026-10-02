package ops

import (
	"testing"
	"time"
)

// Minutes down a check apart, or two (a run may miss one), are one outage.
func TestOutages(t *testing.T) {
	m := func(minute int64) int64 { return 1_790_000_000 + minute*60 }
	down := []struct{ ts, status int64 }{ // newest first
		{m(100), 530}, {m(99), 530}, {m(97), 530}, // one, still going
		{m(50), 0}, // one minute
		{m(10), 502}, {m(9), 502},
	}
	got := outages(down, time.Unix(m(100), 0))
	if len(got) != 3 {
		t.Fatalf("outages %+v", got)
	}
	if !got[0].Ongoing || got[0].Minutes() != 4 || got[0].Status != 530 {
		t.Errorf("latest %+v, %d min", got[0], got[0].Minutes())
	}
	if got[1].Ongoing || got[1].Minutes() != 1 || got[2].Minutes() != 2 {
		t.Errorf("older %+v", got[1:])
	}
}
