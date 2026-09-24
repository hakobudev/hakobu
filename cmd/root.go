package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// version is injected at build time via
// -ldflags "-X github.com/x0ryz/hako/cmd.version=vX.Y.Z" (see .goreleaser.yml);
// a plain `go build` leaves it as "dev".
var version = "dev"

var rootCmd = &cobra.Command{
	Use:     "hako",
	Short:   "Self-hosted deployment tool",
	Version: version,
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
