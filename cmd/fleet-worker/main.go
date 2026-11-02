// Command fleet-worker runs on each machine and executes jobs from the gateway.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"mactunnel/internal/worker"
)

func main() {
	configPath := flag.String("config", "worker.yaml", "path to the worker manifest")
	check := flag.Bool("check", false, "validate the manifest and exit")
	flag.Parse()

	if err := run(*configPath, *check); err != nil {
		fmt.Fprintln(os.Stderr, "fleet-worker:", err)
		os.Exit(1)
	}
}

func run(configPath string, check bool) error {
	manifest, err := worker.LoadManifest(configPath)
	if err != nil {
		return err
	}
	if check {
		fmt.Printf("%s is valid (node %s, %d capabilities)\n", configPath, manifest.Node.Name, len(manifest.Capabilities))
		return nil
	}
	token, err := manifest.ReadToken()
	if err != nil {
		return err
	}
	// Commands get a fresh environment, but drop this anyway as a second safeguard.
	os.Unsetenv("FLEET_NODE_TOKEN")

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	w, err := worker.New(manifest, token, log)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return w.Run(ctx)
}
