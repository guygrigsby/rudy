package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/guygrigsby/rudy/internal/cli"
)

func main() {
	err := cli.NewRoot().ExecuteContext(context.Background())
	if err == nil {
		return
	}
	var ee cli.ExitError
	if errors.As(err, &ee) {
		os.Exit(ee.Code)
	}
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
