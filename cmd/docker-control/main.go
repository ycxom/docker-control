package main

import (
	"os"

	"github.com/ycxom/docker-control/internal/command"
)

var version = "dev"

func main() {
	os.Exit(command.Run(os.Args[1:], version, os.Stdout, os.Stderr))
}
