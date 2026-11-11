package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"text/tabwriter"

	"mactunnel/internal/worker"
)

const toolsUsage = `usage:
  fleet-worker tools templates                        list the built-in templates
  fleet-worker tools new NAME --template TEMPLATE [--config worker.yaml]
                                                      start a tool from a template`

func toolsCommand(args []string) error {
	if len(args) == 0 {
		return errors.New(toolsUsage)
	}
	switch args[0] {
	case "templates":
		return listTemplates(os.Stdout)
	case "new":
		return newTool(args[1:])
	case "-h", "--help", "help":
		fmt.Println(toolsUsage)
		return nil
	}
	return fmt.Errorf("unknown tools command %q\n%s", args[0], toolsUsage)
}

func listTemplates(out io.Writer) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, t := range worker.Templates(runtime.GOOS) {
		fmt.Fprintf(tw, "%s\t%s\n", t.Name, t.Description)
	}
	return tw.Flush()
}

func newTool(args []string) error {
	fs := flag.NewFlagSet("tools new", flag.ContinueOnError)
	template := fs.String("template", "", "template to start from; see `fleet-worker tools templates`")
	configPath := fs.String("config", "worker.yaml", "path to the worker manifest")
	// The name comes first, but flags may be given on either side of it.
	var name string
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		name, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if name == "" && fs.NArg() == 1 {
		name = fs.Arg(0)
	} else if fs.NArg() > 0 {
		return errors.New(toolsUsage)
	}
	if name == "" || *template == "" {
		return errors.New(toolsUsage)
	}
	manifest, err := worker.LoadManifest(*configPath)
	if err != nil {
		return err
	}
	path, err := worker.NewToolFile(manifest, *template, name, runtime.GOOS)
	if err != nil {
		return err
	}
	fmt.Printf("Created %s\n\nFill in each TODO, then set enabled: true. The worker picks it up within seconds.\nTo check it first: fleet-worker --config %s --check\n", path, *configPath)
	return nil
}
