package deploy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
)

// scratchImage is an image with no files at all, made locally: the ingest
// relay is hakobu's own static binary, mounted into it, so nothing has to
// be pulled.
const scratchImage = "hakobu-scratch:1"

// IngestRelayPort is where the relay listens on the projects' networks.
const IngestRelayPort = 8080

// ensureScratchImage imports an empty tarball as scratchImage once.
func ensureScratchImage(ctx context.Context) error {
	if ok, err := ImageExists(ctx, scratchImage); err != nil || ok {
		return err
	}
	// An empty tar archive: two zeroed 512-byte blocks.
	req, err := http.NewRequestWithContext(ctx, "POST", "http://docker/images/create?fromSrc=-&repo=hakobu-scratch&tag=1", bytes.NewReader(make([]byte, 1024)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := dockerClient.Do(req)
	if err != nil {
		return fmt.Errorf("docker daemon unreachable: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("making the empty image failed (%d): %s", resp.StatusCode, body)
	}
	return nil
}

// RunIngestRelay runs the ingest relay as name on network: binary is this
// hakobu, socket the ingest socket it passes envelopes on to. It runs as
// nobody, without capabilities, on a read-only, empty file system.
func RunIngestRelay(ctx context.Context, name, network, binary, socket string) error {
	if err := ensureScratchImage(ctx); err != nil {
		return err
	}
	_, err := runContainer(ctx, name, map[string]any{
		"Image": scratchImage,
		"Cmd":   []string{"/hakobu", "ingest-relay", "/run/ingest/" + filepath.Base(socket), fmt.Sprintf(":%d", IngestRelayPort)},
		"User":  "65534:65534",
	}, map[string]any{
		"NetworkMode":    network,
		"Binds":          []string{binary + ":/hakobu:ro", filepath.Dir(socket) + ":/run/ingest:z"},
		"ReadonlyRootfs": true,
		"SecurityOpt":    []string{"no-new-privileges"},
		"CapDrop":        []string{"ALL"},
		"Memory":         64 << 20,
		"MemorySwap":     64 << 20,
	})
	return err
}
