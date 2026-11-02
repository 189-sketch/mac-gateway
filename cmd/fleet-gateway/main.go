// Command fleet-gateway runs the control plane and manages its credentials.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"mactunnel/internal/gateway"
)

// version is set when building a release.
var version = "dev"

const usage = `usage:
  fleet-gateway version
  fleet-gateway serve [flags]
  fleet-gateway node add <name>      [--data DIR]   print a new node token
  fleet-gateway node revoke <name>   [--data DIR]
  fleet-gateway key add <name> --capabilities 'tts.*,video.*' [--data DIR]   print a new API key
  fleet-gateway key revoke <name>    [--data DIR]`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "fleet-gateway:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "version":
		fmt.Println("fleet-gateway", version)
		return nil
	case "serve":
		return serve(args[1:])
	case "node", "key":
		if len(args) < 3 {
			return errors.New(usage)
		}
		return manageCredential(args[0], args[1], args[2], args[3:])
	default:
		return errors.New(usage)
	}
}

func serve(args []string) error {
	cfg := gateway.DefaultConfig()
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.StringVar(&cfg.Listen, "listen", cfg.Listen, "address to listen on; keep it on loopback behind a TLS proxy")
	fs.StringVar(&cfg.DataDir, "data", cfg.DataDir, "directory for the database and result files")
	fs.DurationVar(&cfg.Heartbeat, "heartbeat", cfg.Heartbeat, "how often workers send heartbeats")
	fs.DurationVar(&cfg.Lease, "lease", cfg.Lease, "silence after which a worker's jobs are considered lost")
	fs.DurationVar(&cfg.LostGrace, "lost-grace", cfg.LostGrace, "how long a lost job without retry waits for its worker")
	fs.DurationVar(&cfg.DeadlineGrace, "deadline-grace", cfg.DeadlineGrace, "extra time past a job's timeout, mainly for uploads")
	fs.IntVar(&cfg.MaxAttempts, "max-attempts", cfg.MaxAttempts, "maximum attempts per job")
	fs.DurationVar(&cfg.Tick, "tick", cfg.Tick, "how often to run maintenance")
	fs.DurationVar(&cfg.ArtifactTTL, "artifact-ttl", cfg.ArtifactTTL, "how long result files are kept")
	fs.Int64Var(&cfg.MaxArtifactBytes, "max-artifact-bytes", cfg.MaxArtifactBytes, "total storage limit for result files")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	store, err := gateway.OpenStore(cfg.DataDir)
	if err != nil {
		return err
	}
	defer store.Close()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	g, err := gateway.New(cfg, store, log)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return g.Run(ctx)
}

func manageCredential(kind, action, name string, args []string) error {
	fs := flag.NewFlagSet(kind, flag.ContinueOnError)
	dataDir := fs.String("data", gateway.DefaultConfig().DataDir, "data directory")
	capabilities := fs.String("capabilities", "", "comma-separated capability patterns, such as tts.*")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		return err
	}
	store, err := gateway.OpenStore(*dataDir)
	if err != nil {
		return err
	}
	defer store.Close()
	ctx := context.Background()

	switch kind + " " + action {
	case "node add":
		token, err := store.AddNode(ctx, name)
		if err != nil {
			return err
		}
		fmt.Println(token)
	case "node revoke":
		return store.RevokeNode(ctx, name)
	case "key add":
		if *capabilities == "" {
			return errors.New("--capabilities is required; use '*' to allow all")
		}
		secret, err := store.AddAPIKey(ctx, name, strings.Split(*capabilities, ","))
		if err != nil {
			return err
		}
		fmt.Println(secret)
	case "key revoke":
		return store.RevokeAPIKey(ctx, name)
	default:
		return errors.New(usage)
	}
	return nil
}
