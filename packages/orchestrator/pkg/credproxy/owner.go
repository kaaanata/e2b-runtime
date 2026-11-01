package credproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"time"
)

// Purpose separates the native login/Remote Control identity from inference.
type Purpose string

const (
	Control   Purpose = "control"
	Inference Purpose = "inference"
)

// ResolveRequest crosses only the private owner socket. Session is a routing
// key, never an authorization or an account selector.
type ResolveRequest struct {
	Provider Provider `json:"provider"`
	Purpose  Purpose  `json:"purpose"`
	Session  string   `json:"session,omitempty"`
	Force    bool     `json:"force,omitzero"`
}

// Lease carries no refresh token. Only the native CLI can renew its account.
type Lease struct {
	Account    string     `json:"account"`
	Credential Credential `json:"credential"`
}

func (Lease) String() string     { return "[credential lease redacted]" }
func (l Lease) GoString() string { return l.String() }

type Outcome struct {
	Provider   Provider `json:"provider"`
	Purpose    Purpose  `json:"purpose"`
	Session    string   `json:"session,omitempty"`
	Account    string   `json:"account"`
	Status     int      `json:"status"`
	Generation string   `json:"generation,omitempty"`
	RetryAfter int      `json:"retry_after_seconds,omitzero"`
}

type Owner interface {
	Resolve(ctx context.Context, request ResolveRequest) (Lease, error)
	Observe(ctx context.Context, outcome Outcome) error
}

type ownerClient struct{ client *http.Client }

func newOwnerClient(socket string) (*ownerClient, error) {
	if !filepath.IsAbs(socket) {
		return nil, errors.New("credential owner socket must be absolute")
	}

	return &ownerClient{client: &http.Client{
		Timeout: 55 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (c *ownerClient) call(ctx context.Context, endpoint string, input, output any) error {
	b, err := json.Marshal(input)
	if err != nil {
		return errors.New("cannot encode credential owner request")
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/v1/"+endpoint, bytes.NewReader(b))
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/json")
	res, err := c.client.Do(r)
	if err != nil {
		return errors.New("credential owner unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return errors.New("credential owner requires attention; inspect its status")
	}
	if output == nil {
		return nil
	}
	b, err = io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if err != nil || len(b) > 1<<20 {
		return errors.New("invalid credential owner response")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if dec.Decode(output) != nil {
		return errors.New("credential owner response violates access-only lease schema")
	}
	if dec.Decode(new(any)) != io.EOF {
		return errors.New("credential owner sent trailing response data")
	}

	return nil
}

func (c *ownerClient) Resolve(ctx context.Context, r ResolveRequest) (Lease, error) {
	var l Lease
	if err := c.call(ctx, "resolve", r, &l); err != nil {
		return Lease{}, err
	}
	if l.Account == "" || l.Credential.Provider != r.Provider || l.Credential.Validate() != nil || !l.Credential.ExpiresAt.After(time.Now()) {
		return Lease{}, errors.New("credential owner returned invalid or expired lease")
	}

	return l, nil
}

func (c *ownerClient) Observe(ctx context.Context, o Outcome) error {
	return c.call(ctx, "outcome", o, nil)
}
