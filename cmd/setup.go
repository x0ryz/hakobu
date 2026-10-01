package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

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
	setupCmd.Flags().BoolVar(&setupReconnect, "reconnect", false, "give hakobu a new Cloudflare API token")
	rootCmd.AddCommand(setupCmd)
}

func runSetup(cmd *cobra.Command, args []string) error {
	if err := config.PrepareDataDir(); err != nil {
		return err
	}
	s, err := store.Open("data/hakobu.db")
	if err != nil {
		return err
	}
	if setupReconnect || !ops.CloudflareConnected(s) {
		if err := connectCloudflare(s); err != nil {
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

// connectCloudflare asks for an API token, through the dashboard form for
// one with hakobu's permissions filled in, or takes CLOUDFLARE_API_TOKEN.
func connectCloudflare(s *store.Store) error {
	if token := os.Getenv("CLOUDFLARE_API_TOKEN"); token != "" {
		_, err := ops.ConnectCloudflare(s, token)
		return err
	}
	host, _ := os.Hostname()
	fmt.Println("\nOpen this link, select Continue to summary and Create Token (the permissions are filled in):")
	fmt.Println("\n  " + cloudflare.TokenTemplateURL("hakobu "+strings.Split(host, ".")[0]))
	fmt.Println("\nCopy the token Cloudflare shows and paste it here (it isn't echoed).")
	for attempt := 0; ; attempt++ {
		fmt.Print("API token: ")
		token, err := readSecret()
		if err != nil {
			return err
		}
		if strings.TrimSpace(token) == "" {
			continue
		}
		if _, err = ops.ConnectCloudflare(s, token); err == nil {
			fmt.Println("Connected.")
			return nil
		}
		fmt.Println(err)
		if attempt == 2 {
			return fmt.Errorf("no working token; run `hakobu setup` again")
		}
	}
}

// readSecret reads a line without echoing it when stdin is a terminal.
func readSecret() (string, error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		return string(b), err
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return line, nil
}

func readLine(r *bufio.Reader) string {
	line, _ := r.ReadString('\n')
	return line
}
