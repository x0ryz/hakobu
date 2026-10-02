package cmd

import (
	"context"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"time"

	"github.com/spf13/cobra"
)

// Apps send their errors, logs and traces to hakobu inside the server: the
// panel's public address goes through Cloudflare, which challenges
// requests from datacenter addresses like the server's own, so an SDK's
// would be refused. hakobu serves the ingest endpoint, and nothing else,
// on a unix socket; the ingest relay, a container on every project's
// network, takes the SDKs' HTTP and passes it on to that socket. Apps get
// SENTRY_DSN pointing at it.

// ingestRelayCmd runs in the relay's container (ops.ensureIngestRelay).
var ingestRelayCmd = &cobra.Command{
	Use:    "ingest-relay <socket> <address>",
	Short:  "Pass apps' Sentry envelopes on to hakobu's ingest socket (run in a container)",
	Args:   cobra.ExactArgs(2),
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		srv := &http.Server{
			Addr:              args[1],
			Handler:           ingestRelay(args[0]),
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       time.Minute,
			WriteTimeout:      time.Minute,
			IdleTimeout:       2 * time.Minute,
		}
		return srv.ListenAndServe()
	},
}

func init() {
	rootCmd.AddCommand(ingestRelayCmd)
}

// envelopePath is the only thing the relay passes on.
var envelopePath = regexp.MustCompile(`^/api/[0-9]+/envelope/?$`)

// ingestRelay proxies envelope requests to the unix socket.
func ingestRelay(socket string) http.Handler {
	target, _ := url.Parse("http://hakobu")
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
		MaxIdleConns:    16,
		IdleConnTimeout: time.Minute,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !envelopePath.MatchString(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}
