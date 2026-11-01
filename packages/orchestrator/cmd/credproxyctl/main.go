//go:build unix

// credproxyctl is an SSH-friendly client to the orchestrator's private socket.
// Native login belongs to credowner; stdout contains only masked guest files.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	flags := flag.NewFlagSet("credproxyctl", flag.ContinueOnError)
	socket := flags.String("socket", "/var/lib/e2b/credproxy/control.sock", "private node socket")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 2 || flags.Arg(0) != "guest" {
		return errors.New("usage: credproxyctl [--socket PATH] guest claude|codex; login is managed by credowner")
	}
	provider := flags.Arg(1)
	if provider != "claude" && provider != "codex" {
		return errors.New("unknown provider")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", *socket)
	}}}
	endpoint := "http://unix/guest/" + provider
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("credential proxy control connection failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("credential proxy control HTTP %d", resp.StatusCode)
	}
	_, err = io.Copy(os.Stdout, io.LimitReader(resp.Body, 1<<20))

	return err
}
