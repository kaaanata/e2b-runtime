package credproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestTLSHTTP2AndStreaming(t *testing.T) {
	t.Parallel()
	p, _ := testProxy(t)
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	release := make(chan struct{})
	defer close(release)
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer synthetic-access-control:org" {
			t.Error("TLS request not injected")
		}
		reader, writer := io.Pipe()
		go func() {
			defer writer.Close()
			_, _ = fmt.Fprint(writer, "data: first\n\n")
			select {
			case <-release:
			case <-ctx.Done():
			}
		}()
		res := response("")
		res.Body = reader
		res.Header.Set("Content-Type", "text/event-stream")

		return res, nil
	})
	go func() {
		conn, e := ln.Accept()
		if e == nil {
			p.ServeConn(ctx, conn, "api.anthropic.com", rt, "sandbox")
		}
	}()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM([]byte(p.CABundle()))
	tr := &http.Transport{ForceAttemptHTTP2: true, TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", ln.Addr().String())
	}}
	defer tr.CloseIdleConnections()
	r, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.anthropic.com/v1/events", nil)
	r.Header.Set("Authorization", "Bearer "+ClaudeAccess)
	res, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b := make([]byte, len("data: first\n\n"))
	if _, err = io.ReadFull(res.Body, b); err != nil || string(b) != "data: first\n\n" || res.ProtoMajor != 2 {
		t.Fatal("h2 or immediate streaming failed", err)
	}
}

func TestWebSocketPreservesScopedRemoteToken(t *testing.T) {
	t.Parallel()
	p, o := testProxy(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer rc-session" {
			t.Error("RC token changed")
		}
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)

			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		_ = buf.Flush()
		payload := make([]byte, 4)
		if _, err = io.ReadFull(buf, payload); err == nil {
			_, _ = buf.Write(payload)
			_ = buf.Flush()
		}
	}))
	defer up.Close()
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		u, err := url.Parse(up.URL)
		if err != nil {
			return nil, err
		}
		r.URL.Scheme = u.Scheme
		r.URL.Host = u.Host

		return http.DefaultTransport.RoundTrip(r)
	})
	gw := httptest.NewServer(p.Handler("chatgpt.com", rt, "sandbox"))
	defer gw.Close()
	u, _ := url.Parse(gw.URL)
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	r, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://chatgpt.com/backend-api/wham/remote/control/server", nil)
	r.Header.Set("Authorization", "Bearer rc-session")
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	if err = r.Write(conn); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	res, err := http.ReadResponse(reader, r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusSwitchingProtocols {
		t.Fatal(res.StatusCode)
	}
	_, _ = io.WriteString(conn, "ping")
	b := make([]byte, 4)
	if _, err = io.ReadFull(reader, b); err != nil || string(b) != "ping" {
		t.Fatal("WebSocket stream failed", err)
	}
	if len(o.requests) != 0 {
		t.Fatal("scoped RC token consulted owner")
	}
}
