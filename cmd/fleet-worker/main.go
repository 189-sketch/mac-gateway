// Command fleet-worker runs on each machine and executes jobs from the gateway.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"mactunnel/internal/worker"
)

// version is set when building a release.
var version = "dev"

const maxLogBytes = 10 << 20

func main() {
	configPath := flag.String("config", "worker.yaml", "path to the worker manifest")
	check := flag.Bool("check", false, "validate the manifest and exit")
	logFile := flag.String("log-file", "", "append logs to this file instead of stderr, for running with no console")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("fleet-worker", version)
		return
	}

	if err := run(*configPath, *logFile, *check); err != nil {
		fmt.Fprintln(os.Stderr, "fleet-worker:", err)
		os.Exit(1)
	}
}

func run(configPath, logFile string, check bool) error {
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

	logOutput, err := openLog(logFile)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(logOutput, nil))
	w, err := worker.New(manifest, token, log)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return w.Run(ctx)
}

// openLog keeps one old log, so a long-running machine can't fill its disk.
func openLog(path string) (io.Writer, error) {
	if path == "" {
		return os.Stderr, nil
	}
	if info, err := os.Stat(path); err == nil && info.Size() > maxLogBytes {
		if err := os.Rename(path, path+".1"); err != nil {
			return nil, fmt.Errorf("rotate log: %w", err)
		}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open log: %w", err)
	}
	return file, nil
}
