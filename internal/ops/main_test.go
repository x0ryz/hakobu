package ops

import (
	"fmt"
	"os"
	"testing"

	"github.com/x0ryz/hakobu/internal/deploy"
)

// TestMain gives the shared containers and networks names of their own, so
// tests never touch a real hakobu's Postgres, RustFS, cloudflared or
// networks on the same Docker.
func TestMain(m *testing.M) {
	id := fmt.Sprint(os.Getpid())
	PostgresContainer = "zt-postgres-" + id
	rustfsContainer = "zt-rustfs-" + id
	tunnelContainer = "zt-cloudflared-" + id
	deploy.NetworkName = "zt-hakobu-" + id
	deploy.EdgeNetwork = "zt-hakobu-edge-" + id
	code := m.Run()
	for _, n := range []string{deploy.NetworkName, deploy.EdgeNetwork} {
		_ = deploy.RemoveNetwork(ctx(), n) // only there if a test used it
	}
	os.Exit(code)
}
