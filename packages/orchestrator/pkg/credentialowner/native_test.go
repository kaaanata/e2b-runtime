package credentialowner

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/credproxy"
)

// The test binary stands in for the native executables, exercising real pipes,
// process cleanup, argv, isolated homes and native persistence without a login.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && (os.Args[1] == "auth" || os.Args[1] == "app-server" || os.Args[1] == "login") {
		nativeFixtureMain()

		return
	}
	m.Run()
}

func nativeFixtureMain() { os.Exit(nativeFixture()) }

func fixtureJSON(value any) []byte {
	b, err := json.Marshal(value)
	if err != nil {
		panic("invalid native process test fixture")
	}

	return b
}

func nativeFixture() int {
	if code, ok := loginFixture(); ok {
		return code
	}
	if os.Args[1] == "auth" {
		if len(os.Args) != 3 || os.Args[2] != "login" || os.Getenv("CLAUDE_CODE_OAUTH_REFRESH_TOKEN") != "refresh-private" {
			return 20
		}
		if os.Getenv("ANTHROPIC_BASE_URL") != "" || os.Getenv("HTTPS_PROXY") != "" {
			return 21
		}
		path := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), ".credentials.json")
		b, err := os.ReadFile(path)
		if err != nil {
			return 22
		}
		var doc map[string]json.RawMessage
		if json.Unmarshal(b, &doc) != nil {
			return 23
		}
		var oauth map[string]json.RawMessage
		if json.Unmarshal(doc["claudeAiOauth"], &oauth) != nil {
			return 24
		}
		oauth["accessToken"] = fixtureJSON("native-new-access")
		oauth["refreshToken"] = fixtureJSON("native-new-refresh")
		oauth["expiresAt"] = fixtureJSON(time.Now().Add(time.Hour).UnixMilli())
		doc["claudeAiOauth"] = fixtureJSON(oauth)
		b = fixtureJSON(doc)
		if os.WriteFile(path, b, 0o600) != nil {
			return 25
		}

		return 0
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
			Params struct {
				Include bool `json:"includeToken"`
				Refresh bool `json:"refreshToken"`
			} `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			return 30
		}
		switch request.Method {
		case "initialize":
			if err := encoder.Encode(map[string]any{"id": request.ID, "result": map[string]string{"userAgent": "fixture"}}); err != nil {
				return 37
			}
		case "initialized":
		case "getAuthStatus":
			if request.Params.Include || !request.Params.Refresh {
				return 31
			}
			path := filepath.Join(os.Getenv("CODEX_HOME"), "auth.json")
			b, err := os.ReadFile(path)
			if err != nil {
				return 32
			}
			var doc map[string]json.RawMessage
			if json.Unmarshal(b, &doc) != nil {
				return 33
			}
			var tokens map[string]string
			if json.Unmarshal(doc["tokens"], &tokens) != nil {
				return 34
			}
			payload := fixtureJSON(map[string]int64{"exp": time.Now().Add(time.Hour).Unix()})
			tokens["access_token"] = token(string(payload))
			tokens["refresh_token"] = "native-new-refresh"
			doc["tokens"] = fixtureJSON(tokens)
			b = fixtureJSON(doc)
			if os.WriteFile(path, b, 0o600) != nil {
				return 35
			}
			if err := encoder.Encode(map[string]any{"id": request.ID, "result": map[string]any{"authMethod": "chatgpt", "authToken": nil, "requiresOpenaiAuth": true}}); err != nil {
				return 38
			}
		default:
			return 36
		}
	}

	return 0
}

func TestNativeProcessRenewalContracts(t *testing.T) {
	t.Parallel()
	for _, provider := range []credproxy.Provider{credproxy.Claude, credproxy.Codex} {
		t.Run(string(provider), func(t *testing.T) {
			t.Parallel()
			m := testManager(t, provider)
			a := m.accounts["control"]
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			a.config.Binary = binary
			writeFixture(t, a, "control", time.Now().Add(-time.Minute))
			m.runner = commandRunner{}
			if _, err = m.current(t.Context(), "control", true); err != nil {
				t.Fatal(err)
			}
			state, err := readNative(a.config, a.home)
			if err != nil || state.refresh != "native-new-refresh" {
				t.Fatal("native CLI did not own durable rotation", err)
			}
			if !state.credential.ExpiresAt.After(time.Now().Add(50 * time.Minute)) {
				t.Fatal("native renewed expiry missing")
			}
		})
	}
}

func TestNativeRefreshFailureDoesNotReturnChildOutput(t *testing.T) {
	t.Parallel()
	m := testManager(t, credproxy.Claude)
	a := m.accounts["control"]
	binary, _ := os.Executable()
	a.config.Binary = binary
	err := (commandRunner{}).Run(t.Context(), a.config, a.home, nativeState{refresh: "unrecognized-secret", credential: credproxy.Credential{Scope: "user:inference"}}, false, nil, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("failed native process accepted")
	}
}
