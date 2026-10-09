package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ozgurcd/nuntius"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := nuntius.CLI(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
