package statusline

import (
	"encoding/json"
	"regexp"
	"testing"
)

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func TestRender(t *testing.T) {
	var in Input
	_ = json.Unmarshal([]byte(`{"model":{"id":"claude-opus-5[1m]","display_name":"Opus 5"},"workspace":{"current_dir":"/src/proj"},"context_window":{"used_percentage":86.6}}`), &in)
	git := func(string) (string, int, bool) { return "main", 3, true }
	got := Render(in, "router→ollama", git)
	if plain := ansi.ReplaceAllString(got, ""); plain != "Opus 5 · router→ollama · 87% ctx · main +3 · proj" {
		t.Errorf("plain %q", plain)
	}
	if !regexp.MustCompile(`\x1b\[31m87% ctx`).MatchString(got) {
		t.Errorf("≥85%% should be red: %q", got)
	}
	bare := Render(Input{CWD: "/x/y"}, "", func(string) (string, int, bool) { return "", 0, false })
	if ansi.ReplaceAllString(bare, "") != "y" {
		t.Errorf("bare %q", bare)
	}
}

func TestSessionProvider(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_PROVIDER", "")
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:9/p/openai")
	if got := SessionProvider(); got != "router→openai" {
		t.Errorf("router: %q", got)
	}
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("HARNESS_PROVIDER", "subscription")
	if got := SessionProvider(); got != "subscription" {
		t.Errorf("pin: %q", got)
	}
}
