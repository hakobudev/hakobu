package main

import (
	"github.com/hakobudev/hakobu/cmd"
	"github.com/hakobudev/hakobu/internal/deploy"
)

func main() {
	deploy.RunDialerIfChild()
	cmd.Execute()
}
