package cmd

import (
	"github.com/spf13/cobra"

	"github.com/x0ryz/hakobu/internal/deploy"
)

// dialerCmd runs as the Docker user's service when Docker runs as a user of
// its own (install.sh starts it inside RootlessKit's namespaces); hakobu
// reaches containers through it.
var dialerCmd = &cobra.Command{
	Use:    "dialer <socket>",
	Short:  "Serve hakobu's connections to containers (run by the Docker user)",
	Args:   cobra.ExactArgs(1),
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return deploy.ServeDialerSocket(args[0])
	},
}

func init() {
	rootCmd.AddCommand(dialerCmd)
}
