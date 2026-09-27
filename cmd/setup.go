package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/x0ryz/hakobu/internal/cloudflare"
	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/ops"
	"github.com/x0ryz/hakobu/internal/store"
)

var setupReconnect bool

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Connect Cloudflare and choose the panel's address (run by install.sh)",
	RunE:  runSetup,
}

func init() {
	setupCmd.Flags().BoolVar(&setupReconnect, "reconnect", false, "sign in to Cloudflare again")
	rootCmd.AddCommand(setupCmd)
}

func runSetup(cmd *cobra.Command, args []string) error {
	if config.CloudflareClientID == "" {
		return fmt.Errorf("this build has no Cloudflare OAuth client (set HAKOBU_CF_CLIENT_ID)")
	}
	if err := config.PrepareDataDir(); err != nil {
		return err
	}
	s, err := store.Open("data/hakobu.db")
	if err != nil {
		return err
	}
	if setupReconnect || !ops.CloudflareConnected(s) {
		if err := cloudflareLogin(s); err != nil {
			return err
		}
	}
	if ops.TunnelReady(s) {
		fmt.Println("Cloudflare is connected, panel: https://" + config.PublicHost())
		return nil
	}

	zones, err := ops.Zones(s)
	if err != nil {
		return err
	}
	if len(zones) == 0 {
		return fmt.Errorf("the connected Cloudflare account has no active domains; add one to Cloudflare and run `hakobu setup` again")
	}
	in := bufio.NewReader(os.Stdin)
	zone := zones[0]
	if len(zones) > 1 {
		fmt.Println("\nWhich domain should hakobu use?")
		for i, z := range zones {
			fmt.Printf("  %d) %s\n", i+1, z.Name)
		}
		for {
			fmt.Printf("Choice [1-%d]: ", len(zones))
			n, err := strconv.Atoi(strings.TrimSpace(readLine(in)))
			if err == nil && n >= 1 && n <= len(zones) {
				zone = zones[n-1]
				break
			}
		}
	}
	fmt.Printf("Panel address: <subdomain>.%s [hakobu]: ", zone.Name)
	sub := strings.TrimSpace(readLine(in))
	if sub == "" {
		sub = "hakobu"
	}

	host, err := ops.SetupTunnel(s, zone.ID, sub)
	if err != nil {
		return err
	}
	fmt.Println("Created the tunnel and https://" + host)
	return nil
}

// cloudflareLogin runs the OAuth login: the user authorizes in the browser,
// Cloudflare hands the code to the relay, and this polls the relay for it.
func cloudflareLogin(s *store.Store) error {
	verifier, challenge := cloudflare.PKCE()
	state, err := ops.RandomHex(24)
	if err != nil {
		return err
	}
	redirect := config.CloudflareRelay + "/cf/callback"
	fmt.Println("\nOpen this link, sign in to Cloudflare and select Authorize:")
	fmt.Println("\n  " + cloudflare.AuthorizeURL(config.CloudflareClientID, redirect, state, challenge))
	fmt.Print("\nWaiting for authorization...")

	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		code, ok, err := cloudflare.Poll(config.CloudflareRelay, state)
		if err != nil {
			fmt.Println()
			return err
		}
		if !ok {
			continue
		}
		token, err := cloudflare.Exchange(config.CloudflareClientID, redirect, code, verifier)
		if err != nil {
			fmt.Println()
			return err
		}
		fmt.Println(" connected.")
		return ops.SaveCloudflareToken(s, token)
	}
	fmt.Println()
	return fmt.Errorf("timed out waiting for Cloudflare authorization; run `hakobu setup` again")
}

func readLine(r *bufio.Reader) string {
	line, _ := r.ReadString('\n')
	return line
}
