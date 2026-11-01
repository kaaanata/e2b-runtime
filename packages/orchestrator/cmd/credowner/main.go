//go:build unix

// credowner is the sole native CLI credential manager, outside the orchestrator
// and every sandbox. Its non-login outputs never contain credential values.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/credentialowner"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	flags := flag.NewFlagSet("credowner", flag.ContinueOnError)
	configPath := flags.String("config", "/etc/e2b/credential-owner.json", "private native owner configuration")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	if flags.NArg() < 1 || flags.NArg() > 3 {
		return errors.New("usage: credowner [--config PATH] serve|status|login PROVIDER ACCOUNT")
	}
	config, err := credentialowner.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if flags.Arg(0) == "status" {
		if flags.NArg() != 1 {
			return errors.New("status takes no account")
		}

		return status(ctx, filepath.Join(config.Root, "owner.sock"))
	}
	manager, err := credentialowner.Open(config)
	if err != nil {
		return err
	}
	switch flags.Arg(0) {
	case "serve":
		if runtime.GOOS != "linux" {
			return errors.New("native credential service requires Linux file-backed login homes")
		}
		if flags.NArg() != 1 {
			return errors.New("serve takes no account")
		}

		return manager.Serve(ctx)
	case "login":
		if runtime.GOOS != "linux" {
			return errors.New("native owner login requires Linux; desktop credential stores are never used")
		}
		if flags.NArg() != 3 {
			return errors.New("login requires provider and configured account alias")
		}
		matched := false
		for _, a := range config.Accounts {
			if a.Name == flags.Arg(2) && string(a.Provider) == flags.Arg(1) {
				matched = true
			}
		}
		if !matched {
			return errors.New("native account alias does not match provider")
		}

		return manager.Login(ctx, flags.Arg(2), os.Stdin, os.Stdout, os.Stderr)
	default:
		return errors.New("unknown credential owner operation")
	}
}

func status(ctx context.Context, socket string) error {
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	defer client.CloseIdleConnections()
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix/v1/status", nil)
	if err != nil {
		return err
	}
	res, err := client.Do(r)
	if err != nil {
		return errors.New("native credential owner service is unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return errors.New("native credential owner status failed")
	}
	var states []credentialowner.AccountStatus
	if json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&states) != nil {
		return errors.New("invalid native owner status")
	}

	return json.NewEncoder(os.Stdout).Encode(states)
}
