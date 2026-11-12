package opencode

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactConfig(t *testing.T) {
	in := map[string]any{
		"mcp": map[string]any{
			"x": map[string]any{"environment": map[string]any{"K": "secretval"}},
			"y": map[string]any{"headers": map[string]any{"Authorization": "Bearer secretval"}},
		},
		"provider": map[string]any{"harness-anthropic": map[string]any{"options": map[string]any{"apiKey": "{env:HARNESS_OPENCODE_API_KEY}"}}},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	got := redactConfig(string(b))
	var out map[string]any
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatal(err)
	}
	mcp := out["mcp"].(map[string]any)
	if mcp["x"].(map[string]any)["environment"].(map[string]any)["K"] != "<redacted>" {
		t.Errorf("mcp.x.environment not redacted: %v", mcp["x"])
	}
	if mcp["y"].(map[string]any)["headers"].(map[string]any)["Authorization"] != "<redacted>" {
		t.Errorf("mcp.y.headers not redacted: %v", mcp["y"])
	}
	provider := out["provider"].(map[string]any)
	apiKey := provider["harness-anthropic"].(map[string]any)["options"].(map[string]any)["apiKey"]
	if apiKey != "{env:HARNESS_OPENCODE_API_KEY}" {
		t.Errorf("provider.options.apiKey must not be touched, got %v", apiKey)
	}

	if got := redactConfig("{not json"); got != "{not json" {
		t.Errorf("malformed input must be returned unchanged, got %q", got)
	}
}

// TestRedactConfigProviderOptionsHeaders: provider.<name>.options.headers values must be blanked even
// with no "mcp" section in the config at all (opencode's headless/BYOK configs carry no mcp key).
func TestRedactConfigProviderOptionsHeaders(t *testing.T) {
	in := map[string]any{"provider": map[string]any{
		"x": map[string]any{"options": map[string]any{"headers": map[string]any{"h": "sk-abc123"}}},
	}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	got := redactConfig(string(b))
	if strings.Contains(got, "sk-abc123") {
		t.Errorf("secret leaked into redacted config: %s", got)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["mcp"]; ok {
		t.Errorf("no mcp key was present in the input; output must not invent one: %v", out)
	}
	headers := out["provider"].(map[string]any)["x"].(map[string]any)["options"].(map[string]any)["headers"].(map[string]any)
	if headers["h"] != "<redacted>" {
		t.Errorf("provider.x.options.headers.h = %v, want <redacted>", headers["h"])
	}
}

// TestRedactConfigNonSecretFields: model ids and baseURL pass through untouched.
func TestRedactConfigNonSecretFields(t *testing.T) {
	in := map[string]any{
		"model": "harness-anthropic/claude-opus-5",
		"provider": map[string]any{
			"harness-anthropic": map[string]any{
				"options": map[string]any{"baseURL": "https://api.anthropic.com/v1"},
			},
		},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	got := redactConfig(string(b))
	var out map[string]any
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatal(err)
	}
	if out["model"] != "harness-anthropic/claude-opus-5" {
		t.Errorf("model = %v, must survive unchanged", out["model"])
	}
	opts := out["provider"].(map[string]any)["harness-anthropic"].(map[string]any)["options"].(map[string]any)
	if opts["baseURL"] != "https://api.anthropic.com/v1" {
		t.Errorf("baseURL = %v, must survive unchanged", opts["baseURL"])
	}
}

// redactConfig must blank a secret-naming .command flag's value (bare or "=" form) and a .url's whole query
// string, while leaving non-secret flags, the bare command name, and the flag names themselves intact.
func TestRedactConfigCommandAndURL(t *testing.T) {
	in := map[string]any{"mcp": map[string]any{
		"x": map[string]any{"command": []any{"srv", "--api-key", "SECRET1", "--token=SECRET2", "--verbose", "--private-key=-----BEGIN\nSECRET4\n-----END"}},
		"y": map[string]any{"url": "https://h/p?token=SECRET3"},
		"z": map[string]any{"url": "https://u:SECRET5@${HOST}/mcp?token=SECRET6"}, // net/url rejects the template
	}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	got := redactConfig(string(b))
	for _, secret := range []string{"SECRET1", "SECRET2", "SECRET3", "SECRET4", "SECRET5", "SECRET6"} {
		if strings.Contains(got, secret) {
			t.Errorf("%s leaked into redacted config: %s", secret, got)
		}
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatal(err)
	}
	mcp := out["mcp"].(map[string]any)
	cmd := mcp["x"].(map[string]any)["command"].([]any)
	wantCmd := []any{"srv", "--api-key", "<redacted>", "--token=<redacted>", "--verbose", "--private-key=<redacted>"}
	if len(cmd) != len(wantCmd) {
		t.Fatalf("command = %v, want %v", cmd, wantCmd)
	}
	for i, want := range wantCmd {
		if cmd[i] != want {
			t.Errorf("command[%d] = %v, want %v", i, cmd[i], want)
		}
	}
	if url := mcp["y"].(map[string]any)["url"]; url != "https://h/p?<redacted>" {
		t.Errorf("url = %v, want https://h/p?<redacted>", url)
	}
	if url := mcp["z"].(map[string]any)["url"]; url != "https://${HOST}/mcp?<redacted>" {
		t.Errorf("templated url = %v, want https://${HOST}/mcp?<redacted>", url)
	}
}
