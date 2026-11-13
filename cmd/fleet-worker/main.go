// Command fleet-worker runs on each machine and executes jobs from the gateway.
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
	"runtime/debug"
	"syscall"
	"time"

	"mactunnel/internal/worker"
)

// version is set when building a release.
var version = "dev"

func main() {
	commands := map[string]func([]string) error{"tools": toolsCommand, "ui": uiCommand, "pair": pairCommand}
	if len(os.Args) > 1 && commands[os.Args[1]] != nil {
		err := commands[os.Args[1]](os.Args[2:])
		if errors.Is(err, errSkipped) {
			os.Exit(exitSkipped)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "fleet-worker:", err)
			os.Exit(1)
		}
		return
	}
	configPath := flag.String("config", "worker.yaml", "path to the worker manifest")
	check := flag.Bool("check", false, "validate the manifest, check each capability and exit")
	logFile := flag.String("log-file", "", "also write logs to this file, which rotates by itself")
	logLevel := flag.String("log-level", "", "debug, info, warn or error; overrides log_level in the manifest")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("fleet-worker", version)
		return
	}
	if *check {
		if err := runCheck(*configPath); err != nil {
			fmt.Fprintln(os.Stderr, "fleet-worker:", err)
			os.Exit(1)
		}
		return
	}

	// Logging starts before anything else can fail, because a worker with no console
	// has nowhere else to say why it won't start.
	output, err := openLog(*logFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fleet-worker:", err)
		os.Exit(1)
	}
	if err := run(*configPath, *logLevel, output); err != nil {
		fmt.Fprintf(output, "time=%s level=ERROR msg=\"worker stopped\" error=%q\n", nowText(), err.Error())
		os.Exit(1)
	}
}

func runCheck(configPath string) error {
	manifest, err := worker.LoadManifest(configPath)
	if err != nil {
		return err
	}
	fmt.Printf("%s is valid (node %s, %d tools)\n", configPath, manifest.Node.Name, len(manifest.Capabilities))
	// A capability that isn't ready is a fact about this moment, not a bad config:
	// the worker reports it and starts offering it once it passes.
	for _, result := range worker.VerifyCapabilities(context.Background(), manifest) {
		if result.Reason == "" {
			fmt.Printf("  ready: %s\n", result.Name)
		} else {
			fmt.Printf("  not ready: %s: %s (%s)\n", result.Name, result.Reason, result.Detail)
		}
	}
	for _, name := range manifest.Disabled {
		fmt.Printf("  disabled: %s\n", name)
	}
	// The worker would run without these, but a check is the time to fix them.
	for _, e := range manifest.ToolErrors {
		fmt.Printf("  invalid: %s\n", e.Error())
	}
	if n := len(manifest.ToolErrors); n > 0 {
		return fmt.Errorf("%d tool file(s) aren't valid and would be skipped", n)
	}
	return nil
}

func run(configPath, levelFlag string, output io.Writer) error {
	manifest, err := worker.LoadManifest(configPath)
	if err != nil {
		return err
	}
	levelText := manifest.LogLevel
	if levelFlag != "" {
		levelText = levelFlag
	}
	level, err := worker.ParseLogLevel(levelText)
	if err != nil {
		return fmt.Errorf("log level: %w", err)
	}
	// A machine with no token starts anyway and waits to be paired, from its
	// page or with `fleet-worker pair`.
	token, err := manifest.ReadToken()
	if err != nil && !errors.Is(err, worker.ErrNoToken) {
		return err
	}
	// Commands get a fresh environment, but drop this anyway as a second safeguard.
	os.Unsetenv("FLEET_NODE_TOKEN")

	// The configuration page shows the newest lines too.
	logs := worker.NewLogRing(1000)
	log := slog.New(slog.NewTextHandler(io.MultiWriter(output, logs), &slog.HandlerOptions{Level: level}))
	log.Info("worker starting", "version", version, "node", manifest.Node.Name, "gateway", manifest.GatewayURL, "log_level", level.String())
	worker.LogToolErrors(log, manifest)
	w, err := worker.New(manifest, token, log)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Changed tools are picked up by themselves. SIGHUP asks for it straight away.
	hangups := make(chan os.Signal, 1)
	signal.Notify(hangups, syscall.SIGHUP)
	defer signal.Stop(hangups)
	go func() {
		for {
			select {
			case <-hangups:
				w.RequestReload()
			case <-ctx.Done():
				return
			}
		}
	}()
	go w.WatchConfig(ctx, configPath, worker.ReloadEvery)
	pageDone := make(chan struct{})
	go func() {
		defer close(pageDone)
		w.ServeUI(ctx, worker.UIOptions{ConfigPath: configPath, Logs: logs, Version: version})
	}()

	err = w.Run(ctx)
	// Let the page remove its sign-in file, so `fleet-worker ui` knows it's gone.
	stop()
	select {
	case <-pageDone:
	case <-time.After(3 * time.Second):
	}
	log.Info("worker stopped")
	return err
}

// openLog writes to the console by default. With a log file it also writes to the
// console if there's a person at one, but not otherwise: under launchd or a
// scheduled task, the console output would just be a second, unrotated copy.
func openLog(path string) (io.Writer, error) {
	if path == "" {
		return os.Stderr, nil
	}
	file, err := worker.OpenRotatingFile(path, worker.LogFileMaxBytes, worker.LogFileKeep)
	if err != nil {
		return nil, fmt.Errorf("open log: %w", err)
	}
	// A panic bypasses the logger and normally goes only to the console. A worker that
	// crashes on every start adds to this file each time, so it gets a limit too.
	crashPath := path + ".crash"
	if info, err := os.Stat(crashPath); err == nil && info.Size() > worker.LogFileMaxBytes {
		_ = os.Rename(crashPath, crashPath+".1")
	}
	if crash, err := os.OpenFile(crashPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		if err := debug.SetCrashOutput(crash, debug.CrashOptions{}); err != nil {
			fmt.Fprintf(file, "time=%s level=WARN msg=\"crashes won't be logged\" error=%q\n", nowText(), err.Error())
		}
		crash.Close()
	}
	if stderrIsTerminal() {
		// The file goes first, so a console that's gone can't stop it being written.
		return io.MultiWriter(file, os.Stderr), nil
	}
	return file, nil
}

func stderrIsTerminal() bool {
	info, err := os.Stderr.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	// /dev/null is a character device too, and it's where a service's output often goes.
	null, err := os.Stat(os.DevNull)
	return err != nil || !os.SameFile(info, null)
}

func nowText() string { return time.Now().Format(time.RFC3339) }
