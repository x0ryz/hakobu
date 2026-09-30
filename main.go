package main

import (
	"github.com/x0ryz/hakobu/cmd"
	"github.com/x0ryz/hakobu/internal/deploy"
)

func main() {
	deploy.RunDialerIfChild()
	cmd.Execute()
}
