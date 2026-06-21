package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/imlach/nightwatch/internal/api"
	"github.com/imlach/nightwatch/internal/inventory"
)

// runServe starts the token-authed HTTP trigger API so a remote scheduler can
// drive drain-shutdown / wake / status. It refuses to start without
// NIGHTWATCH_API_TOKEN (the endpoint can power off nodes - fail closed).
func runServe(inv *inventory.Inventory, args []string) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", envDefault("NIGHTWATCH_LISTEN", ":8080"), "listen address")
	kubeconfig := fs.String("kubeconfig", os.Getenv("NIGHTWATCH_KUBECONFIG"), "path to kubeconfig (empty: default rules, then in-cluster)")
	talosconfig := fs.String("talosconfig", os.Getenv("NIGHTWATCH_TALOSCONFIG"), "path to talosconfig")
	_ = fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := api.Serve(ctx, inv, api.ServeConfig{
		Listen:      *listen,
		Token:       os.Getenv("NIGHTWATCH_API_TOKEN"),
		Kubeconfig:  *kubeconfig,
		Talosconfig: *talosconfig,
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		return 1
	}
	return 0
}
