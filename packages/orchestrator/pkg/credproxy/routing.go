package credproxy

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
)

const maxInferenceBody = 16 << 20

var legacyClaudeUser = regexp.MustCompile(`^(user_.+_account_)([^_]*)(_session_)([^\s]+)$`)

func inferencePath(host string, r *http.Request) bool {
	if r.URL.RawPath != "" {
		return false
	}
	if host == "api.anthropic.com" {
		return r.Method == http.MethodPost && (r.URL.Path == "/v1/messages" || r.URL.Path == "/v1/messages/count_tokens")
	}

	return host == "chatgpt.com" && ((r.Method == http.MethodPost && (r.URL.Path == "/backend-api/codex/responses" || r.URL.Path == "/backend-api/codex/responses/compact")) || (r.Method == http.MethodGet && r.URL.Path == "/backend-api/codex/responses" && strings.EqualFold(r.Header.Get("Upgrade"), "websocket")))
}

type requestRouting struct {
	resolve        ResolveRequest
	claudeBody     map[string]json.RawMessage
	claudeMetadata map[string]json.RawMessage
	claudeUser     string
}

func routeRequest(host, team, sandbox string, r *http.Request) (requestRouting, error) {
	x := requestRouting{resolve: ResolveRequest{Provider: provider(host), Purpose: Control}}
	if !inferencePath(host, r) {
		return x, nil
	}
	x.resolve.Purpose = Inference
	session := ""
	for _, key := range []string{"session_id", "x-session-id", "x-codex-session-id"} {
		if value := r.Header.Get(key); value != "" {
			session = value

			break
		}
	}
	if x.resolve.Provider == Claude {
		var reader io.Reader = r.Body
		switch r.Header.Get("Content-Encoding") {
		case "", "identity":
		case "gzip":
			gz, err := gzip.NewReader(r.Body)
			if err != nil {
				return x, errors.New("invalid compressed inference request")
			}
			defer gz.Close()
			reader = gz
		default:
			return x, errors.New("unsupported Claude request encoding for account routing")
		}
		b, err := io.ReadAll(io.LimitReader(reader, maxInferenceBody+1))
		_ = r.Body.Close()
		if err != nil || len(b) > maxInferenceBody || json.Unmarshal(b, &x.claudeBody) != nil || x.claudeBody == nil {
			return x, errors.New("claude account routing requires a bounded JSON request")
		}
		if raw := x.claudeBody["metadata"]; len(raw) > 0 && json.Unmarshal(raw, &x.claudeMetadata) != nil {
			return x, errors.New("invalid Claude request metadata")
		}
		if raw := x.claudeMetadata["user_id"]; len(raw) > 0 {
			if json.Unmarshal(raw, &x.claudeUser) != nil {
				return x, errors.New("invalid Claude caller identity")
			}
			var user struct {
				Session string `json:"session_id"`
			}
			if json.Unmarshal([]byte(x.claudeUser), &user) == nil {
				session = user.Session
			} else if parts := legacyClaudeUser.FindStringSubmatch(x.claudeUser); parts != nil {
				session = parts[4]
			} else {
				return x, errors.New("unrecognized Claude caller identity format")
			}
		}
		// The request is re-encoded after an account has been selected.
	}
	if session == "" {
		// A missing conversation identifier pins the whole sandbox. Compaction
		// cannot use this fallback: its opaque state may be from another run.
		if strings.HasSuffix(r.URL.Path, "/compact") {
			return x, errors.New("native compaction requires a stable conversation identifier")
		}
		if sandbox == "" {
			return x, errors.New("inference lacks trusted sandbox scope")
		}
		session = "sandbox:" + sandbox
	} else {
		session = "conversation:" + session
	}
	if len(session) > 512 || strings.ContainsAny(session, "\x00\r\n") {
		return x, errors.New("invalid conversation identifier")
	}
	sum := sha256.Sum256([]byte(team + "\x00" + string(x.resolve.Provider) + "\x00" + session))
	x.resolve.Session = hex.EncodeToString(sum[:])

	return x, nil
}

func (x *requestRouting) apply(r *http.Request, c Credential) error {
	if c.Provider == Codex {
		r.Header.Set("ChatGPT-Account-ID", c.AccountID)
		// These belong to the caller's original account or connection. They
		// must not be carried into a pooled account's inference request.
		if x.resolve.Purpose == Inference {
			for _, h := range []string{"x-oai-attestation", "x-codex-turn-state", "x-openai-codex-luna-reserve"} {
				r.Header.Del(h)
			}
		}
	}
	if x.claudeBody == nil {
		return nil
	}
	if x.claudeUser != "" {
		account, _, ok := strings.Cut(c.AccountID, ":")
		if !ok {
			return errors.New("claude lease has no account/organization identity")
		}
		var user map[string]json.RawMessage
		if json.Unmarshal([]byte(x.claudeUser), &user) == nil && user != nil {
			encodedAccount, err := json.Marshal(account)
			if err != nil {
				return err
			}
			user["account_uuid"] = encodedAccount
			b, err := json.Marshal(user)
			if err != nil {
				return err
			}
			x.claudeUser = string(b)
		} else if parts := legacyClaudeUser.FindStringSubmatch(x.claudeUser); parts != nil {
			x.claudeUser = parts[1] + account + parts[3] + parts[4]
		} else {
			return errors.New("unsupported Claude caller identity")
		}
		encodedUser, err := json.Marshal(x.claudeUser)
		if err != nil {
			return err
		}
		x.claudeMetadata["user_id"] = encodedUser
		encodedMetadata, err := json.Marshal(x.claudeMetadata)
		if err != nil {
			return err
		}
		x.claudeBody["metadata"] = encodedMetadata
	}
	b, err := json.Marshal(x.claudeBody)
	if err != nil {
		return err
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.ContentLength = int64(len(b))
	r.Header.Del("Content-Encoding")
	r.Header.Del("Content-Length")

	return nil
}
