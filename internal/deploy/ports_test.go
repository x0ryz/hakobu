package deploy

import (
	"slices"
	"testing"
)

func TestParseListening(t *testing.T) {
	tables := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0 100 0 0 10 0
   1: 0B00007F:B4C3 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2 1 0 100 0 0 10 0
   2: 0100007F:0CEA 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 3 1 0 100 0 0 10 0
   3: 0200007F:1770 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 4 1 0 100 0 0 10 0
   4: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 5 1 0 100 0 0 10 0
   5: 0100007F:0050 0100007F:9C40 01 00000000:00000000 00:00000000 00000000     0        0 6 1 0 100 0 0 10 0
  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000001000000:2328 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 7 1 0 100 0 0 10 0
   1: 0000000000000000FFFF00000100007F:2329 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 8 1 0 100 0 0 10 0
   2: 00000000000000000000000000000000:0BB8 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 9 1 0 100 0 0 10 0
`
	reachable, loopback := parseListening(tables)
	// 0.0.0.0:8080 (also on 127.0.0.1) and :::3000; Docker's resolver on
	// 127.0.0.11:46275 is neither, the connection on :80 isn't listening.
	if want := []int{3000, 8080}; !slices.Equal(reachable, want) {
		t.Errorf("reachable = %v, want %v", reachable, want)
	}
	// 127.0.0.1:3306, 127.0.0.2:6000, [::1]:9000, [::ffff:127.0.0.1]:9001.
	if want := []int{3306, 6000, 9000, 9001}; !slices.Equal(loopback, want) {
		t.Errorf("loopback = %v, want %v", loopback, want)
	}
}
