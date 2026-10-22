// Command hakobu v1, without the panel: built with go build -o hakobu ./v1
// until it replaces the panel's hakobu (stage 9 of the move).
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
