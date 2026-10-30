package vendors

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
	got := RedactConfig(string(b))
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

	if got := RedactConfig("{not json"); got != "{not json" {
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
	got := RedactConfig(string(b))
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
	got := RedactConfig(string(b))
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

// RedactConfig must blank a secret-naming .command flag's value (bare or "=" form) and a .url's whole query
// string, while leaving non-secret flags, the bare command name, and the flag names themselves intact.
func TestRedactConfigCommandAndURL(t *testing.T) {
	in := map[string]any{"mcp": map[string]any{
		"x": map[string]any{"command": []any{"srv", "--api-key", "SECRET1", "--token=SECRET2", "--verbose"}},
		"y": map[string]any{"url": "https://h/p?token=SECRET3"},
	}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	got := RedactConfig(string(b))
	for _, secret := range []string{"SECRET1", "SECRET2", "SECRET3"} {
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
	wantCmd := []any{"srv", "--api-key", "<redacted>", "--token=<redacted>", "--verbose"}
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
}
