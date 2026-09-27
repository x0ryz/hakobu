package ops

import "testing"

func TestJobReservation(t *testing.T) {
	if ok, err := reserve("web", false); !ok || err != nil {
		t.Fatalf("first reserve = %v, %v", ok, err)
	}
	if ok, err := reserve("web", false); ok || err == nil {
		t.Error("a second manual deploy should be refused while one runs")
	}
	if ok, err := reserve("web", true); ok || err != nil {
		t.Errorf("a push during a deploy should be queued, got %v, %v", ok, err)
	}
	if !release("web") {
		t.Error("release should report the queued push")
	}
	if IsDeploying("web") {
		t.Error("app still marked busy after release")
	}
	if ok, _ := reserve("web", false); !ok || release("web") {
		t.Error("the queue should be empty after it was handed out")
	}
}

func TestHumanBytes(t *testing.T) {
	for in, want := range map[uint64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KB", 64 << 30: "64.0 GB"} {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}
