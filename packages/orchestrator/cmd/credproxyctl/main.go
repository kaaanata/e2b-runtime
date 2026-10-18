//go:build unix

// credproxyctl is an SSH-friendly client to the orchestrator's private socket.
// import reads secrets from stdin; stdout contains only masked guest files.
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
	replace := flags.Bool("replace", false, "allow replacing a different account")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 2 || (flags.Arg(0) != "import" && flags.Arg(0) != "guest") {
		return errors.New("usage: credproxyctl [--socket PATH] [--replace] import|guest claude|codex")
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
	method := http.MethodGet
	var body io.Reader
	if flags.Arg(0) == "import" {
		method = http.MethodPost
		endpoint = "http://unix/import"
		if *replace {
			endpoint += "?replace=true"
		}
		body = io.LimitReader(os.Stdin, (1<<20)+1)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
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
