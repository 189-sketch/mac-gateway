// Command fleet is the client for agents and people: it submits jobs, follows
// their progress and fetches their results.
//
// Results go to stdout as JSON and progress goes to stderr, so a script can read
// stdout without filtering. Exit codes are listed in the usage text.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"mactunnel/internal/client"
	"mactunnel/internal/transfer"
)

// version is set when building a release.
var version = "dev"

const usage = `usage: fleet <command> [arguments]

  fleet version
  fleet capabilities
  fleet run CAPABILITY [flags]        submit a job, follow it, download its results
  fleet status JOB [--wait N] [--events]
  fleet watch JOB                     print events as JSON lines until the job ends
  fleet cancel JOB
  fleet download JOB [-o PATH] [--force]
  fleet upload PATH

flags for run:
  --input JSON           job input: JSON text, @file, or - for stdin
  -f FIELD=PATH          upload a local file as an input file (repeatable)
  -o PATH                where to save results (default: current directory)
  --force                overwrite existing files
  --selector KEY=VALUE   only run on nodes with this label (repeatable)
  --queue-timeout D      how long to wait for a node, such as 10m
  --idempotency-key KEY  repeat this key to get the same job back
  --detach               print the job and exit without waiting
  --quiet                don't print progress

environment:
  FLEET_URL                    gateway address
  FLEET_API_KEY                API key
  FLEET_ALLOW_INSECURE_HTTP=1  allow plain http to a remote gateway

exit codes:
  0   success
  1   the job or command failed; retrying won't help
  2   invalid usage
  75  a temporary failure; running it again may work
  130 interrupted; a submitted job keeps running`

const (
	exitOK        = 0
	exitFailed    = 1
	exitUsage     = 2
	exitRetry     = 75
	exitInterrupt = 130
)

type usageError struct{ message string }

func (e *usageError) Error() string { return e.message }

// localError is a problem on this machine, such as a file that already exists.
// Running the command again wouldn't help, and might repeat a finished job.
type localError struct{ message string }

func (e *localError) Error() string { return e.message }

func usagef(format string, args ...any) error {
	return &usageError{message: fmt.Sprintf(format, args...)}
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(os.Stderr, usage)
		if len(args) == 0 {
			return exitUsage
		}
		return exitOK
	}
	if args[0] == "version" {
		fmt.Println("fleet", version)
		return exitOK
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	commands := map[string]func(context.Context, []string) (int, error){
		"capabilities": cmdCapabilities,
		"run":          cmdRun,
		"status":       cmdStatus,
		"watch":        cmdWatch,
		"cancel":       cmdCancel,
		"download":     cmdDownload,
		"upload":       cmdUpload,
	}
	command, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(os.Stderr, "fleet: unknown command %q\n\n%s\n", args[0], usage)
		return exitUsage
	}
	code, err := command(ctx, args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "fleet:", err)
		return exitCodeFor(ctx, err)
	}
	return code
}

func exitCodeFor(ctx context.Context, err error) int {
	var usageErr *usageError
	var apiErr *client.Error
	var localErr *localError
	var statusErr *transfer.StatusError
	switch {
	case errors.As(err, &usageErr), errors.Is(err, flag.ErrHelp):
		return exitUsage
	case ctx.Err() != nil:
		return exitInterrupt
	case errors.As(err, &localErr):
		return exitFailed
	case errors.As(err, &statusErr) && statusErr.Code < 500:
		return exitFailed
	case errors.As(err, &apiErr):
		if apiErr.Retryable {
			return exitRetry
		}
		return exitFailed
	default:
		return exitRetry // Most other errors are network failures.
	}
}

func newClient() (*client.Client, error) {
	c, err := client.New(os.Getenv("FLEET_URL"), os.Getenv("FLEET_API_KEY"), os.Getenv("FLEET_ALLOW_INSECURE_HTTP") == "1")
	if err != nil {
		return nil, &usageError{message: err.Error()}
	}
	c.Logf = func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "fleet: "+format+"\n", args...)
	}
	return c, nil
}

func printJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

// jobExitCode turns a finished job into an exit code.
func jobExitCode(job *client.Job) int {
	switch {
	case job.Status == "succeeded":
		return exitOK
	case job.Error != nil && job.Error.Retryable:
		return exitRetry
	default:
		return exitFailed
	}
}

// listFlag collects a repeatable flag.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }
func (l *listFlag) Set(value string) error {
	*l = append(*l, value)
	return nil
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard) // Errors are reported once, by run.
	return fs
}

// parseWithPositional lets a positional argument come before the flags, so that
// both "fleet run tts.say --input ..." and "fleet run --input ... tts.say" work.
func parseWithPositional(fs *flag.FlagSet, args []string, what string) (string, error) {
	var positional string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		positional, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return "", usagef("%v", err)
	}
	if positional == "" && fs.NArg() > 0 {
		positional = fs.Arg(0)
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return "", usagef("%v", err)
		}
	}
	if positional == "" {
		return "", usagef("%s is required", what)
	}
	if fs.NArg() > 0 {
		return "", usagef("unexpected argument %q", fs.Arg(0))
	}
	return positional, nil
}

func parsePairs(pairs []string, what string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || key == "" || value == "" {
			return nil, usagef("%s must look like KEY=VALUE, got %q", what, pair)
		}
		out[key] = value
	}
	return out, nil
}

// readInput resolves --input, which may be JSON text, @file or - for stdin.
func readInput(value string) (json.RawMessage, error) {
	if value == "" {
		return nil, nil
	}
	var raw []byte
	var err error
	switch {
	case value == "-":
		raw, err = io.ReadAll(os.Stdin)
	case strings.HasPrefix(value, "@"):
		raw, err = os.ReadFile(value[1:])
	default:
		raw = []byte(value)
	}
	if err != nil {
		return nil, usagef("read --input: %v", err)
	}
	if !json.Valid(raw) {
		return nil, usagef("--input is not valid JSON")
	}
	return raw, nil
}

func randomKey() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "fleet-cli-" + hex.EncodeToString(b)
}

func cmdRun(ctx context.Context, args []string) (int, error) {
	fs := newFlagSet("run")
	inputFlag := fs.String("input", "", "")
	output := fs.String("o", "", "")
	force := fs.Bool("force", false, "")
	queueTimeout := fs.String("queue-timeout", "", "")
	idempotencyKey := fs.String("idempotency-key", "", "")
	detach := fs.Bool("detach", false, "")
	quiet := fs.Bool("quiet", false, "")
	var fileFlags, selectorFlags listFlag
	fs.Var(&fileFlags, "f", "")
	fs.Var(&selectorFlags, "selector", "")
	capability, err := parseWithPositional(fs, args, "CAPABILITY")
	if err != nil {
		return 0, err
	}
	input, err := readInput(*inputFlag)
	if err != nil {
		return 0, err
	}
	localFiles, err := parsePairs(fileFlags, "-f")
	if err != nil {
		return 0, err
	}
	selector, err := parsePairs(selectorFlags, "--selector")
	if err != nil {
		return 0, err
	}
	c, err := newClient()
	if err != nil {
		return 0, err
	}
	for _, path := range localFiles {
		if _, err := os.Stat(path); err != nil {
			return 0, usagef("-f: %v", err)
		}
	}

	var uploaded map[string]string
	if len(localFiles) > 0 {
		uploaded = make(map[string]string, len(localFiles))
		for field, path := range localFiles {
			progressf(*quiet, "uploading %s", path)
			file, err := c.UploadFile(ctx, path)
			if err != nil {
				return 0, fmt.Errorf("upload %s: %w", path, err)
			}
			uploaded[field] = file.ID
		}
	}

	// A generated key makes it safe to retry the submit after a lost response,
	// because the gateway returns the same job instead of creating a second one.
	key := *idempotencyKey
	if key == "" {
		key = randomKey()
	}
	job, err := c.CreateJob(ctx, client.NewJob{
		Capability:     capability,
		Input:          input,
		Files:          uploaded,
		Selector:       selector,
		QueueTimeout:   *queueTimeout,
		IdempotencyKey: key,
	})
	if err != nil {
		return 0, err
	}
	if *detach {
		return exitOK, printJSON(job)
	}
	progressf(*quiet, "job %s submitted", job.ID)

	final, err := c.Watch(ctx, job.ID, 0, func(ev client.Event) {
		if !*quiet {
			printEvent(ev)
		}
	}, func(j *client.Job) {
		progressf(*quiet, "status: %s", j.Status)
	})
	if err != nil {
		if ctx.Err() != nil {
			fmt.Fprintf(os.Stderr, "fleet: stopped following; job %s keeps running. Use `fleet status %s` or `fleet cancel %s`.\n", job.ID, job.ID, job.ID)
			return exitInterrupt, nil
		}
		return 0, fmt.Errorf("follow job %s: %w", job.ID, err)
	}

	saved, err := saveArtifacts(ctx, c, final, *output, *force)
	if err != nil {
		// Never exit with "retry" here: the job worked, and rerunning the command
		// would run the job again rather than fetch its results.
		_ = printJSON(runResult{Job: final, Saved: saved})
		fmt.Fprintf(os.Stderr, "fleet: job %s succeeded, but its results weren't saved: %v. Use `fleet download %s` to try again.\n", final.ID, err, final.ID)
		return exitFailed, nil
	}
	if err := printJSON(runResult{Job: final, Saved: saved}); err != nil {
		return 0, err
	}
	return jobExitCode(final), nil
}

// runResult is what `fleet run` prints: the job and where its results went.
type runResult struct {
	*client.Job
	Saved []savedFile `json:"saved,omitempty"`
}

type savedFile struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

func progressf(quiet bool, format string, args ...any) {
	if !quiet {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	}
}

func printEvent(ev client.Event) {
	if ev.Type != "progress" {
		fmt.Fprintf(os.Stderr, "event: %s\n", ev.Type)
		return
	}
	var p client.Progress
	if json.Unmarshal(ev.Payload, &p) != nil {
		return
	}
	parts := []string{}
	if p.Stage != "" {
		parts = append(parts, p.Stage)
	}
	if p.Percent != nil {
		parts = append(parts, strconv.FormatFloat(*p.Percent, 'f', -1, 64)+"%")
	}
	if p.Current != "" {
		parts = append(parts, p.Current)
	}
	if p.Message != "" {
		parts = append(parts, p.Message)
	}
	fmt.Fprintln(os.Stderr, "progress:", strings.Join(parts, " "))
}

// artifactNameRE guards local paths. Names come from the gateway, and a hostile
// one must not be able to write outside the chosen directory.
var artifactNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// destinations works out where each result file goes. With a single result, -o
// can name the file itself. Otherwise -o is a directory.
func destinations(artifacts []client.Artifact, output string, force bool) ([]string, error) {
	paths := make([]string, len(artifacts))
	dir := output
	single := false
	if output != "" {
		info, err := os.Stat(output)
		isDir := err == nil && info.IsDir()
		single = !isDir && len(artifacts) == 1 && !strings.HasSuffix(output, string(os.PathSeparator))
	} else {
		dir = "."
	}
	for i, artifact := range artifacts {
		if !artifactNameRE.MatchString(artifact.Name) {
			return nil, &localError{message: fmt.Sprintf("refusing unsafe result file name %q", artifact.Name)}
		}
		if single {
			paths[i] = output
		} else {
			paths[i] = filepath.Join(dir, artifact.Name)
		}
		if !force {
			if _, err := os.Lstat(paths[i]); err == nil {
				return nil, &localError{message: fmt.Sprintf("%s already exists; use --force to overwrite it", paths[i])}
			}
		}
	}
	return paths, nil
}

func saveArtifacts(ctx context.Context, c *client.Client, job *client.Job, output string, force bool) ([]savedFile, error) {
	if len(job.Artifacts) == 0 {
		return nil, nil
	}
	paths, err := destinations(job.Artifacts, output, force)
	if err != nil {
		return nil, err
	}
	saved := make([]savedFile, 0, len(paths))
	for i, artifact := range job.Artifacts {
		if err := os.MkdirAll(filepath.Dir(paths[i]), 0o755); err != nil {
			return saved, &localError{message: err.Error()}
		}
		if err := c.Download(ctx, artifact, paths[i]); err != nil {
			return saved, fmt.Errorf("download %s: %w", artifact.Name, err)
		}
		saved = append(saved, savedFile{Name: artifact.Name, Path: paths[i]})
	}
	return saved, nil
}

func singleArg(fs *flag.FlagSet, args []string, what string) (string, error) {
	return parseWithPositional(fs, args, what)
}

func cmdCapabilities(ctx context.Context, args []string) (int, error) {
	if len(args) > 0 {
		return 0, usagef("unexpected argument %q", args[0])
	}
	c, err := newClient()
	if err != nil {
		return 0, err
	}
	raw, err := c.Capabilities(ctx)
	if err != nil {
		return 0, err
	}
	return exitOK, printJSON(raw)
}

func cmdStatus(ctx context.Context, args []string) (int, error) {
	fs := newFlagSet("status")
	wait := fs.Int("wait", 0, "")
	events := fs.Bool("events", false, "")
	id, err := singleArg(fs, args, "JOB")
	if err != nil {
		return 0, err
	}
	c, err := newClient()
	if err != nil {
		return 0, err
	}
	job, err := c.GetJob(ctx, id, *wait, *events)
	if err != nil {
		return 0, err
	}
	// A status check itself worked, whatever the job's state is.
	return exitOK, printJSON(job)
}

func cmdWatch(ctx context.Context, args []string) (int, error) {
	id, err := singleArg(newFlagSet("watch"), args, "JOB")
	if err != nil {
		return 0, err
	}
	c, err := newClient()
	if err != nil {
		return 0, err
	}
	encoder := json.NewEncoder(os.Stdout)
	final, err := c.Watch(ctx, id, 0, func(ev client.Event) {
		_ = encoder.Encode(ev)
	}, func(j *client.Job) {
		_ = encoder.Encode(map[string]any{"type": "status", "status": j.Status, "attempts": j.Attempts})
	})
	if err != nil {
		if ctx.Err() != nil {
			return exitInterrupt, nil
		}
		return 0, err
	}
	if err := encoder.Encode(map[string]any{"type": "end", "job": final}); err != nil {
		return 0, err
	}
	return jobExitCode(final), nil
}

func cmdCancel(ctx context.Context, args []string) (int, error) {
	id, err := singleArg(newFlagSet("cancel"), args, "JOB")
	if err != nil {
		return 0, err
	}
	c, err := newClient()
	if err != nil {
		return 0, err
	}
	job, err := c.CancelJob(ctx, id)
	if err != nil {
		return 0, err
	}
	return exitOK, printJSON(job)
}

func cmdDownload(ctx context.Context, args []string) (int, error) {
	fs := newFlagSet("download")
	output := fs.String("o", "", "")
	force := fs.Bool("force", false, "")
	id, err := singleArg(fs, args, "JOB")
	if err != nil {
		return 0, err
	}
	c, err := newClient()
	if err != nil {
		return 0, err
	}
	job, err := c.GetJob(ctx, id, 0, false)
	if err != nil {
		return 0, err
	}
	if len(job.Artifacts) == 0 {
		return 0, &client.Error{Code: "NO_RESULTS", Message: fmt.Sprintf("job %s has no results (status: %s)", id, job.Status), Retryable: !job.Terminal}
	}
	saved, err := saveArtifacts(ctx, c, job, *output, *force)
	if err != nil {
		return 0, err
	}
	return exitOK, printJSON(map[string]any{"saved": saved})
}

func cmdUpload(ctx context.Context, args []string) (int, error) {
	path, err := singleArg(newFlagSet("upload"), args, "PATH")
	if err != nil {
		return 0, err
	}
	if _, err := os.Stat(path); err != nil {
		return 0, usagef("%v", err)
	}
	c, err := newClient()
	if err != nil {
		return 0, err
	}
	file, err := c.UploadFile(ctx, path)
	if err != nil {
		return 0, err
	}
	return exitOK, printJSON(file)
}
