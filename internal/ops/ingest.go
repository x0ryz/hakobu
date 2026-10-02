package ops

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/x0ryz/hakobu/internal/deploy"
	"github.com/x0ryz/hakobu/internal/store"
)

// Apps reach hakobu's ingest endpoint inside the server, through the ingest
// relay (cmd/ingest_relay.go): a container on every project's network that
// passes envelopes on to IngestSocket.

// IngestSocketDir holds the ingest socket; only the relay mounts it.
const IngestSocketDir = "run/ingest"

// IngestSocket serves the ingest endpoint and nothing else.
func IngestSocket() string { return filepath.Join(IngestSocketDir, "ingest.sock") }

// ingestContainer is the relay; apps reach it by this name.
var ingestContainer = "hakobu-ingest"

// ingestNetwork is the relay's own network; it joins the projects' too.
var ingestNetwork = "hakobu-ingest"

// ingestDSN is the SENTRY_DSN of an app on this server.
func ingestDSN(key string, appID int64) string {
	return fmt.Sprintf("http://%s@%s:%d/%d", key, ingestContainer, deploy.IngestRelayPort, appID)
}

// KeepIngestRelay starts the relay with this hakobu's binary, trying again
// until Docker answers.
func KeepIngestRelay(s *store.Store) {
	for wait := 5 * time.Second; ; wait = min(2*wait, 5*time.Minute) {
		err := startIngestRelay(s)
		if err == nil {
			return
		}
		fmt.Printf("ingest relay not started (trying again in %s): %v\n", wait, err)
		time.Sleep(wait)
	}
}

// startIngestRelay runs the relay afresh, so it runs the binary of this
// hakobu, and connects it to the projects' networks.
func startIngestRelay(s *store.Store) error {
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	if bin, err = filepath.EvalSymlinks(bin); err != nil {
		return err
	}
	sock, err := filepath.Abs(IngestSocket())
	if err != nil {
		return err
	}
	if err := deploy.EnsureNetwork(ctx(), ingestNetwork); err != nil {
		return err
	}
	if err := deploy.RunIngestRelay(ctx(), ingestContainer, ingestNetwork, bin, sock); err != nil {
		return err
	}
	return ensureAllProjectNetworks(s)
}
