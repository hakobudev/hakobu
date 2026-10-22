// Command hakobu deploys projects to servers you own, from the hakobu.toml
// in each repository.
package main

import (
	"fmt"
	"os"

	"github.com/hakobudev/hakobu/internal/cli"
	"github.com/hakobudev/hakobu/internal/deploy"
)

// version is set by GoReleaser via -ldflags.
var version = "dev"

func main() {
	deploy.RunDialerIfChild() // rootless Docker: this binary is the dialer too
	if err := cli.Root(version).Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
