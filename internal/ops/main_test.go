package ops

import (
	"fmt"
	"os"
	"testing"

	"github.com/x0ryz/hakobu/internal/deploy"
)

// TestMain gives the shared containers and networks names of their own, so
// tests never touch a real hakobu's Postgres, cloudflared or
// networks on the same Docker.
func TestMain(m *testing.M) {
	deploy.RunDialerIfChild() // rootless Docker: this binary is the dialer too
	id := fmt.Sprint(os.Getpid())
	PostgresContainer = "zt-postgres-" + id
	tunnelContainer = "zt-cloudflared-" + id
	ingestContainer, ingestNetwork = "zt-ingest-"+id, "zt-ingest-"+id
	deploy.NetworkName = "zt-hakobu-" + id
	deploy.EdgeNetwork = "zt-hakobu-edge-" + id
	code := m.Run()
	for _, n := range []string{deploy.NetworkName, deploy.EdgeNetwork, ingestNetwork} {
		_ = deploy.RemoveNetwork(ctx(), n) // only there if a test used it
	}
	os.Exit(code)
}
