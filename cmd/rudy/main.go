package main

import (
	"os"

	"github.com/guygrigsby/rudy/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		os.Exit(1)
	}
}
