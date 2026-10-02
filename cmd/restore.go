package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/x0ryz/hakobu/internal/cloudflare"
	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/ops"
)

var (
	restoreKey string
	restoreAt  string
)

// restoreCmd brings a panel back on a new server from its backup in R2,
// before `hakobu setup` (install.sh runs it when HAKOBU_RESTORE is set).
var restoreCmd = &cobra.Command{
	Use:   "restore",
	Short: "Restore the panel from its backup, with the key file downloaded from Settings",
	RunE:  runRestore,
}

func init() {
	restoreCmd.Flags().StringVar(&restoreKey, "key", "", "the key file from Settings → Master key (- for stdin)")
	restoreCmd.Flags().StringVar(&restoreAt, "at", "", "restore the backup made at this time (YYYYMMDD-HHMMSS, UTC) instead of the newest")
	_ = restoreCmd.MarkFlagRequired("key")
	rootCmd.AddCommand(restoreCmd)
}

func runRestore(cmd *cobra.Command, args []string) error {
	if err := config.PrepareDataDir(); err != nil {
		return err
	}
	var in io.Reader = os.Stdin
	if restoreKey != "-" {
		f, err := os.Open(restoreKey)
		if err != nil {
			return err
		}
		defer f.Close()
		in = f
	}
	kf, err := ops.ParseKeyFile(in)
	if err != nil {
		return err
	}
	token := strings.TrimSpace(os.Getenv("CLOUDFLARE_API_TOKEN"))
	if token == "" {
		if restoreKey == "-" {
			return fmt.Errorf("set CLOUDFLARE_API_TOKEN: the key file comes on stdin")
		}
		fmt.Print("Cloudflare API token that can read R2 (it isn't echoed): ")
		if token, err = readSecret(); err != nil {
			return err
		}
		if token = strings.TrimSpace(token); token == "" {
			return errNoSecret
		}
	}
	at, err := ops.RestorePanel(cloudflare.Client{Token: token, AccountID: kf.AccountID}, kf, config.DatabaseFile, config.MasterKeyFile, restoreAt)
	if err != nil {
		return err
	}
	fmt.Printf("Restored the panel of %s as it was at %s.\n", kf.PublicHost, at.UTC().Format("2006-01-02 15:04 UTC"))
	fmt.Println("Its apps are redeployed from GitHub by Redeploy; databases and volumes come back from their backups (database page or app page → Restore).")
	return nil
}
