// Command fleet-gateway runs the control plane and manages its credentials.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

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
  fleet-gateway key add <name> --dashboard [--data DIR]   print a new key for viewing the dashboard
  fleet-gateway key revoke <name>    [--data DIR]
  fleet-gateway pair [--data DIR] [--wait 15m] [--until-connected]   show the codes of machines asking to pair`

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
	case "pair":
		return pair(args[1:], os.Stdout)
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
	cfg.Version = version
	fs.StringVar(&cfg.PublicURL, "public-url", cfg.PublicURL, "where agents reach the gateway, such as https://fleet.example.com, for links in MCP results; by default it's taken from the proxy's headers")
	fs.DurationVar(&cfg.MCPWait, "mcp-wait", cfg.MCPWait, "how long an MCP tool call waits for its job before answering with the job's ID")
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
	dashboard := fs.Bool("dashboard", false, "make a key that can only view the dashboard")
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
		if *dashboard && *capabilities != "" {
			return errors.New("a dashboard key can't run jobs, so don't combine --dashboard with --capabilities")
		}
		var secret string
		var err error
		switch {
		case *dashboard:
			secret, err = store.AddDashboardKey(ctx, name)
		case *capabilities == "":
			return errors.New("--capabilities is required; use '*' to allow all")
		default:
			secret, err = store.AddAPIKey(ctx, name, strings.Split(*capabilities, ","))
		}
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

// pair shows the code of each machine that asks to pair, as it asks, and says
// when one has connected. Codes are shown here and nowhere else: being able to
// run this on the server is what proves a person may add a machine.
func pair(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	dataDir := fs.String("data", gateway.DefaultConfig().DataDir, "data directory")
	wait := fs.Duration("wait", 15*time.Minute, "how long to keep watching; 0 lists what's waiting now and exits")
	untilConnected := fs.Bool("until-connected", false, "stop as soon as a machine has connected")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if _, err := os.Stat(*dataDir); err != nil {
		return fmt.Errorf("the gateway's data isn't in %s: %w", *dataDir, err)
	}
	store, err := gateway.OpenStore(*dataDir)
	if err != nil {
		return err
	}
	defer store.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	started := time.Now()
	deadline := started.Add(*wait)
	shown := map[string]bool{}
	paired := map[string]bool{}
	connected := map[string]bool{}
	for {
		pairings, err := store.Pairings(ctx, started)
		if err != nil {
			return err
		}
		for _, p := range pairings {
			if p.Paired {
				if !paired[p.ID] {
					paired[p.ID] = true
					fmt.Fprintf(out, "==> Paired %s. It's connecting now.\n", p.NodeName)
				}
				continue
			}
			if shown[p.ID] {
				continue
			}
			shown[p.ID] = true
			what := p.Name
			if p.OS != "" {
				what += " (" + p.OS + "/" + p.Arch + ")"
			}
			fmt.Fprintf(out, "\n==> %s at %s is asking to pair. Type this code on that machine:\n\n        %s\n\n    It works for %d minutes. If you didn't start this, ignore it.\n",
				what, p.Remote, gateway.FormatPairingCode(p.Code), int(time.Until(p.Expires).Round(time.Minute).Minutes()))
		}
		nodes, err := store.NodesSeenSince(ctx, started)
		if err != nil {
			return err
		}
		for _, n := range nodes {
			if connected[n.Name] {
				continue
			}
			connected[n.Name] = true
			tools := fmt.Sprintf("%d tools", n.Tools)
			if n.Tools == 1 {
				tools = "1 tool"
			}
			fmt.Fprintf(out, "==> %s is connected, offering %s.\n", n.Name, tools)
		}
		if *untilConnected && len(connected) > 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			if *wait > 0 && len(connected) == 0 {
				fmt.Fprintln(out, "No machine has connected yet. Run this again when you're ready to pair one.")
			} else if *wait == 0 && len(shown) == 0 {
				fmt.Fprintln(out, "No machine is asking to pair.")
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
	}
}
