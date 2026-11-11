package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"mactunnel/internal/worker"
)

// uiCommand opens the running worker's configuration page, signed in.
func uiCommand(args []string) error {
	flags := flag.NewFlagSet("ui", flag.ContinueOnError)
	configPath := flags.String("config", "worker.yaml", "path to the worker manifest")
	noOpen := flags.Bool("no-open", false, "print the address instead of opening a browser")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "usage: fleet-worker ui [--config worker.yaml] [--no-open]\n\nOpens the running worker's configuration page. The address works once, for two minutes.")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	manifest, err := worker.LoadManifest(*configPath)
	if err != nil {
		return err
	}
	if manifest.UIAddress() == "" {
		return fmt.Errorf("the configuration page is off; remove ui_listen: off from %s and restart the worker", *configPath)
	}
	state, err := worker.ReadUIState(manifest.StateDir)
	if errors.Is(err, fs.ErrNotExist) {
		return errors.New("the worker isn't running, or its page couldn't start (see the worker's log); start the worker first")
	}
	if err != nil {
		return fmt.Errorf("read the page's address: %w (this needs to run as the same user as the worker)", err)
	}
	code, err := signInCode(state)
	if err != nil {
		return err
	}
	url := state.URL + "#code=" + code
	if *noOpen {
		fmt.Println(url)
		return nil
	}
	if !hasDesktop() {
		// A server has no browser to open, so say how to reach the page instead.
		fmt.Println("This machine has no desktop. From your computer, forward the page's port over SSH:")
		host := "127.0.0.1:7878"
		if parsed, err := neturl.Parse(state.URL); err == nil && parsed.Port() != "" {
			host = parsed.Host
		}
		_, port, _ := strings.Cut(host, "]:")
		if port == "" {
			_, port, _ = strings.Cut(host, ":")
		}
		fmt.Printf("\n  ssh -L %s:%s USER@THIS_MACHINE\n\n", port, host)
		fmt.Println("Then open this address there (it works once, for two minutes):")
		fmt.Println(url)
		return nil
	}
	fmt.Println("Opening", state.URL)
	fmt.Println("If no browser opens, open this address on this machine (it works once, for two minutes):")
	fmt.Println(url)
	if err := openBrowser(url); err != nil {
		fmt.Fprintln(os.Stderr, "fleet-worker: couldn't open a browser:", err)
	}
	return nil
}

func signInCode(state worker.UIState) (string, error) {
	req, err := http.NewRequest(http.MethodPost, state.URL+"api/sign-in-code", strings.NewReader("{}"))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+state.Token)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("the worker's page at %s didn't answer, so the worker probably isn't running: %w", state.URL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var out struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &out)
	if resp.StatusCode != http.StatusOK || out.Code == "" {
		if out.Error == "" {
			out.Error = resp.Status
		}
		return "", fmt.Errorf("the worker's page refused: %s", out.Error)
	}
	return out.Code, nil
}

// hasDesktop reports whether a browser could open here. On Linux and other
// Unix systems, that needs a graphical session.
func hasDesktop() bool {
	switch runtime.GOOS {
	case "darwin", "windows":
		return true
	}
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}
