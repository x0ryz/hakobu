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
	"os"
	"path/filepath"
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

	sock, err := listenPanelSocket()
	if err != nil {
		return err
	}

	go runProxyPoller(s)
	go ops.WatchOOM(s)
	go runBackupScheduler(s)
	if err := ops.StartTunnel(s); err != nil {
		fmt.Println("failed to start the tunnel:", err)
	}

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
		Handler:      edge.Router(s, panelHandler(mux)),
		ReadTimeout:  config.ReadTimeout,
		WriteTimeout: config.WriteTimeout,
		IdleTimeout:  config.IdleTimeout,
	}
	go func() {
		if err := srv.Serve(sock); err != nil {
			fmt.Println("panel socket:", err)
		}
	}()
	return srv.Serve(ln)
}

// listenPanelSocket is where cloudflared, in its own container, reaches the
// panel: a unix socket in a directory mounted into it. The socket is open to
// every local user, like the loopback port.
func listenPanelSocket() (net.Listener, error) {
	if err := os.MkdirAll(ops.PanelSocketDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.Chmod(ops.PanelSocketDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(ops.PanelSocketDir, "panel.sock")
	os.Remove(path) // left by the previous run
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	return ln, os.Chmod(path, 0o666)
}

// ensureSetupToken keeps a one-time setup token on disk until an owner has
// signed in; only the holder of the setup link can connect GitHub and claim
// the panel.
func ensureSetupToken(s *store.Store) (string, error) {
	if owner, err := s.Owner(context.Background()); err != nil || owner.GitHubID != 0 {
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

// runProxyPoller keeps every app's proxy listening (after an agent restart
// this re-attaches them to their containers) and retries tunnel route
// updates that failed.
func runProxyPoller(s *store.Store) {
	lastErr := ""
	for {
		if apps, err := s.ListApps(context.Background()); err == nil {
			for _, a := range apps {
				if !ops.IsDeploying(a.Name) { // a running job (or deletion) owns the proxy
					ops.EnsureProxy(a)
				}
			}
		}
		err := ops.SyncTunnel(s)
		if err != nil && err.Error() != lastErr {
			fmt.Println("tunnel routes not updated (retrying):", err)
		}
		lastErr = fmt.Sprint(err)
		time.Sleep(config.ProxyPollEvery)
	}
}

// runBackupScheduler checks hourly for databases due a backup (so restarts
// don't postpone them) and cleans up once a day.
func runBackupScheduler(s *store.Store) {
	time.Sleep(time.Minute) // let Docker and the databases come up first
	lastCleanup := time.Now()
	for ; ; time.Sleep(time.Hour) {
		for name, err := range ops.BackupDue(s) {
			if err != nil {
				fmt.Println("backup failed for", name+":", err)
			}
		}
		if time.Since(lastCleanup) < 24*time.Hour {
			continue
		}
		lastCleanup = time.Now()
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
