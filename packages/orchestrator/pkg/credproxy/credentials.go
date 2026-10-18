// Package credproxy implements an opt-in, single-owner credential boundary.
// Real OAuth credentials stay on the node; guests receive only sentinels.
package credproxy

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Provider string

const (
	Claude        Provider = "claude"
	Codex         Provider = "codex"
	ClaudeAccess           = "sk-ant-oat01-sbx-proxy-managed"
	ClaudeRefresh          = "sk-ant-ort01-sbx-proxy-managed"
	CodexAccess            = "oai-access-sbx-proxy-managed"
	CodexRefresh           = "oai-refresh-sbx-proxy-managed"
)

// Credential is the private node-side wire format, sent only over SSH/Unix socket.
// Do not log or format it. State holds public Claude onboarding metadata only.
type Credential struct {
	Provider         Provider        `json:"provider"`
	AccountID        string          `json:"account_id"`
	AccessToken      string          `json:"access_token"`
	RefreshToken     string          `json:"refresh_token"`
	IDToken          string          `json:"id_token,omitempty"`
	ExpiresAt        time.Time       `json:"expires_at"`
	Scope            string          `json:"scope,omitempty"`
	SubscriptionType string          `json:"subscription_type,omitempty"`
	RateLimitTier    string          `json:"rate_limit_tier,omitempty"`
	State            json.RawMessage `json:"state,omitempty"`
}

func (c Credential) String() string   { return "[credential redacted]" }
func (c Credential) GoString() string { return c.String() }
func (p Provider) Valid() bool        { return p == Claude || p == Codex }
func (p Provider) AccessSentinel() string {
	if p == Claude {
		return ClaudeAccess
	}

	return CodexAccess
}

func (p Provider) RefreshSentinel() string {
	if p == Claude {
		return ClaudeRefresh
	}

	return CodexRefresh
}

func (c Credential) Validate() error {
	if !c.Provider.Valid() || c.AccountID == "" || c.AccessToken == "" || c.RefreshToken == "" || c.ExpiresAt.IsZero() {
		return errors.New("incomplete credential")
	}
	if strings.Contains(c.AccessToken, "proxy-managed") || strings.Contains(c.RefreshToken, "proxy-managed") {
		return errors.New("cannot import guest sentinels")
	}
	if c.Provider == Codex {
		if _, err := publicIDToken(c.IDToken); err != nil {
			return err
		}
	}

	return nil
}

// GuestFiles contains no provider bearer or refresh token. It is safe to persist
// on the workspace volume. It still contains account metadata, so use mode 0600.
type GuestFiles struct {
	Credentials json.RawMessage `json:"credentials"`
	State       json.RawMessage `json:"state,omitempty"`
}

func (c Credential) GuestFiles() (GuestFiles, error) {
	if c.Provider == Claude {
		raw, err := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{
			"accessToken": ClaudeAccess, "refreshToken": ClaudeRefresh, "expiresAt": c.ExpiresAt.UnixMilli(), "scopes": strings.Fields(c.Scope), "subscriptionType": c.SubscriptionType, "rateLimitTier": c.RateLimitTier,
		}})

		return GuestFiles{Credentials: raw, State: c.State}, err
	}
	id, err := publicIDToken(c.IDToken)
	if err != nil {
		return GuestFiles{}, err
	}
	raw, err := json.Marshal(map[string]any{"auth_mode": "chatgpt", "OPENAI_API_KEY": nil, "tokens": map[string]string{"access_token": CodexAccess, "refresh_token": CodexRefresh, "id_token": id, "account_id": c.AccountID}, "last_refresh": time.Now().UTC().Format(time.RFC3339)})

	return GuestFiles{Credentials: raw}, err
}

// Codex parses identity/plan claims locally. Never copy the signed ID token into
// the VM: emit an explicitly unsigned metadata document with an allowlist.
func publicIDToken(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("invalid Codex ID token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", errors.New("invalid Codex ID token")
	}
	var claims map[string]json.RawMessage
	if json.Unmarshal(payload, &claims) != nil {
		return "", errors.New("invalid Codex ID token")
	}
	allowed := make(map[string]json.RawMessage)
	for _, key := range []string{"email", "sub", "https://api.openai.com/auth", "https://api.openai.com/profile"} {
		if v, ok := claims[key]; ok {
			allowed[key] = v
		}
	}

	if auth, ok := allowed["https://api.openai.com/auth"]; ok {
		var fields map[string]json.RawMessage
		if json.Unmarshal(auth, &fields) != nil {
			return "", errors.New("invalid Codex auth claims")
		}
		public := make(map[string]json.RawMessage)
		for _, key := range []string{"chatgpt_account_id", "chatgpt_user_id", "chatgpt_plan_type", "user_id", "organizations"} {
			if value, ok := fields[key]; ok {
				public[key] = value
			}
		}
		encoded, err := json.Marshal(public)
		if err != nil {
			return "", errors.New("cannot encode identity metadata")
		}
		allowed["https://api.openai.com/auth"] = encoded
	}
	raw, err := json.Marshal(allowed)
	if err != nil {
		return "", fmt.Errorf("encode identity metadata: %w", err)
	}

	return "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0." + base64.RawURLEncoding.EncodeToString(raw) + ".c2J4LXByb3h5LW1hbmFnZWQ", nil
}

// Some Codex token responses express expiry only in the access JWT.
func accessExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.Exp <= 0 {
		return time.Time{}
	}

	return time.Unix(claims.Exp, 0)
}
