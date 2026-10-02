package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// version is set by GoReleaser via -ldflags.
var version = "dev"

var rootCmd = &cobra.Command{
	Use:     "hakobu",
	Short:   "Self-hosted deployment tool",
	Version: version,
	// Usage is for mistakes in the command line; once a command runs, its
	// errors stand alone.
	PersistentPreRun: func(cmd *cobra.Command, args []string) { cmd.SilenceUsage = true },
	SilenceErrors:    true, // Execute prints them
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.AddCommand(agentCmd)
}
