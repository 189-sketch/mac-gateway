package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"mactunnel/internal/worker"
)

// exitSkipped is how `fleet-worker pair` says the person chose to pair later,
// which an installer treats differently from a failure.
const exitSkipped = 3

var errSkipped = errors.New("skipped")

// pairCommand connects this machine to a gateway from a terminal. The installers
// run it, and it can be run by hand at any time; a running worker notices the
// result and connects without a restart.
func pairCommand(args []string) error {
	flags := flag.NewFlagSet("pair", flag.ContinueOnError)
	configPath := flags.String("config", "worker.yaml", "path to the worker manifest")
	gateway := flags.String("gateway", "", "the gateway's address, such as 203.0.113.10 or fleet.example.com")
	key := flags.String("key", "", "a node token made on the server with fleet-add-worker, instead of a pairing code")
	name := flags.String("name", "", "what to call this machine; the default is its hostname")
	code := flags.String("code", "", "the pairing code, to skip being asked for it")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "usage: fleet-worker pair [--config worker.yaml] [--gateway ADDRESS] [--key TOKEN]\n\nConnects this machine to a gateway. It asks the gateway to pair, then asks you for the code the server shows.")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	manifest, err := worker.LoadManifest(*configPath)
	if err != nil {
		return err
	}
	in := bufio.NewReader(os.Stdin)
	ask := func(prompt string) (string, error) {
		fmt.Print(prompt)
		line, err := in.ReadString('\n')
		if err != nil && (err != io.EOF || line == "") {
			fmt.Println()
			return "", errSkipped
		}
		return strings.TrimSpace(line), nil
	}
	ctx := context.Background()

	address := *gateway
	if address == "" {
		address = manifest.GatewayURL
	}
	if address == "" {
		if address, err = ask("Gateway address (the server's IP address or domain; press Enter to skip): "); err != nil || address == "" {
			return errSkipped
		}
	}
	gatewayURL, err := worker.NormalizeGatewayURL(address, manifest.AllowInsecureHTTP)
	if err != nil {
		return err
	}

	nodeName := *name
	if nodeName == "" {
		nodeName = manifest.Node.Name
	}
	if nodeName == "" {
		nodeName = worker.DefaultNodeName()
	}

	if *key != "" {
		callCtx, cancel := context.WithTimeout(ctx, worker.PairTimeout)
		defer cancel()
		issuedFor, err := worker.IdentifyToken(callCtx, gatewayURL, strings.TrimSpace(*key))
		if err != nil {
			return err
		}
		return finishPairing(*configPath, gatewayURL, issuedFor, strings.TrimSpace(*key))
	}

	callCtx, cancel := context.WithTimeout(ctx, worker.PairTimeout)
	request, err := worker.RequestPairing(callCtx, gatewayURL, nodeName)
	cancel()
	if err != nil {
		return err
	}
	fmt.Printf(`
Asked %s to pair this machine as %q.

The server is now showing a pairing code for it. If the gateway's installer is
still waiting there, the code is on its screen. Otherwise run this on the server:

    sudo fleet-pair

`, gatewayURL, request.Name)
	for {
		entered := *code
		if entered == "" {
			if entered, err = ask("Pairing code (press Enter to skip and pair later from the worker's page): "); err != nil || entered == "" {
				return errSkipped
			}
		}
		callCtx, cancel := context.WithTimeout(ctx, worker.PairTimeout)
		issuedFor, token, left, err := worker.ConfirmPairing(callCtx, request, entered)
		cancel()
		switch {
		case errors.Is(err, worker.ErrWrongCode) && *code == "":
			fmt.Printf("That code isn't right. Check it against the one the server shows. %d tries left.\n", left)
			continue
		case errors.Is(err, worker.ErrPairingExpired):
			return errors.New("that pairing request ran out of time or tries. Run this again for a new code")
		case err != nil:
			return err
		}
		return finishPairing(*configPath, gatewayURL, issuedFor, token)
	}
}

func finishPairing(configPath, gatewayURL, nodeName, token string) error {
	if err := worker.SavePairing(configPath, gatewayURL, nodeName, token); err != nil {
		return err
	}
	fmt.Printf("Paired with %s as %s.\n", gatewayURL, nodeName)
	return nil
}
