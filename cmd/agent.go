package cmd

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/edge"
	"github.com/x0ryz/hakobu/internal/ops"
	"github.com/x0ryz/hakobu/internal/store"
)

var (
	agentAddr       string
	agentPublicHost string
)

var agentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Run the hakobu agent (panel, deploys, webhooks)",
	RunE:  runAgent,
}

func init() {
	agentCmd.Flags().StringVar(&agentAddr, "addr", config.AgentAddr, "listen address (or HAKOBU_ADDR)")
	agentCmd.Flags().StringVar(&agentPublicHost, "public-host", "", "public host of the panel, e.g. panel.example.com (saved to data/public_host)")
}

func runAgent(cmd *cobra.Command, args []string) error {
	if err := config.PrepareDataDir(); err != nil {
		return err
	}
	s, err := store.Open("data/hakobu.db")
	if err != nil {
		return err
	}
	if agentPublicHost != "" {
		if err := config.SetPublicHost(agentPublicHost); err != nil {
			return err
		}
	}
	if err := s.FailRunningDeployLogs(context.Background()); err != nil {
		return err
	}

	setupURL, err := ensureSetupToken(s)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook/github", webhookHandler(s))
	registerWebRoutes(mux, s)
	registerIngestRoutes(mux, s)

	ln, err := net.Listen("tcp", agentAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w (is another hakobu agent running?)", agentAddr, err)
	}

	go runProxyPoller(s)
	go runBackupScheduler(s)
	ops.StartTunnel(s)

	fmt.Println("hakobu listening on", ln.Addr())
	switch {
	case config.PublicHost() == "":
		fmt.Println("No panel address yet: run `hakobu setup`.")
	case setupURL != "":
		fmt.Println("Finish setup:", setupURL)
	default:
		fmt.Println("Panel: https://" + config.PublicHost())
	}
	srv := &http.Server{
		Handler:      edge.Router(s, mux),
		ReadTimeout:  config.ReadTimeout,
		WriteTimeout: config.WriteTimeout,
		IdleTimeout:  config.IdleTimeout,
	}
	return srv.Serve(ln)
}

// ensureSetupToken keeps a one-time setup token on disk until an owner has
// signed in; only the holder of the setup link can connect GitHub and claim
// the panel.
func ensureSetupToken(s *store.Store) (string, error) {
	if owner, err := s.Owner(context.Background()); err != nil || owner != "" {
		return "", err
	}
	token := config.SetupToken()
	if token == "" {
		var err error
		if token, err = ops.RandomHex(16); err != nil {
			return "", err
		}
		if err := config.SetSetupToken(token); err != nil {
			return "", err
		}
	}
	return "https://" + config.PublicHost() + "/setup?token=" + token, nil
}

// runProxyPoller keeps every app's proxy listening; after an agent restart
// this re-attaches them to their running containers.
func runProxyPoller(s *store.Store) {
	for {
		if apps, err := s.ListApps(context.Background()); err == nil {
			for _, a := range apps {
				if !ops.IsDeploying(a.Name) { // a running job (or deletion) owns the proxy
					ops.EnsureProxy(a)
				}
			}
		}
		time.Sleep(config.ProxyPollEvery)
	}
}

func runBackupScheduler(s *store.Store) {
	for range time.Tick(config.BackupEvery) {
		for name, err := range ops.BackupAll(s) {
			if err != nil {
				fmt.Println("backup failed for", name+":", err)
			}
		}
		if err := s.PruneOldData(context.Background(), config.RetentionDays); err != nil {
			fmt.Println("prune failed:", err)
		}
		if err := ops.Cleanup(s); err != nil {
			fmt.Println("cleanup failed:", err)
		}
	}
}

// webhookHandler redeploys every app built from the pushed repo when the
// push is to its default branch.
func webhookHandler(s *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		app, err := s.GetGitHubApp(context.Background())
		if err != nil {
			http.Error(w, "github app not connected", http.StatusServiceUnavailable)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 25<<20))
		if err != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}
		mac := hmac.New(sha256.New, []byte(app.WebhookSecret))
		mac.Write(body)
		if !hmac.Equal([]byte("sha256="+hex.EncodeToString(mac.Sum(nil))), []byte(r.Header.Get("X-Hub-Signature-256"))) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("X-GitHub-Event") != "push" {
			return
		}

		var push struct {
			Ref        string `json:"ref"`
			Repository struct {
				FullName      string `json:"full_name"`
				DefaultBranch string `json:"default_branch"`
			} `json:"repository"`
		}
		if err := json.Unmarshal(body, &push); err != nil {
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		if push.Ref != "refs/heads/"+push.Repository.DefaultBranch {
			return
		}
		apps, err := s.ListAppsByRepo(context.Background(), push.Repository.FullName)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, a := range apps {
			if err := ops.StartDeploy(s, a.Name, "push"); err != nil {
				fmt.Println("push deploy of", a.Name, "skipped:", err)
			}
		}
	}
}
