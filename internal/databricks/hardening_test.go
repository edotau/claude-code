package databricks

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestApiKeyHelperQuotesPathWithSpace(t *testing.T) {
	o := base(t, TargetClaude, "serving")
	o.Self = "/opt/my tools/claude-code"
	r, err := Render(o)
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		K string `json:"apiKeyHelper"`
	}
	if err := json.Unmarshal([]byte(r.Text), &d); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(d.K, "'/opt/my tools/claude-code' resolve --profile ") {
		t.Errorf("helper path not quoted: %q", d.K)
	}
	o.Self = "/bin/cc"
	if r, _ = Render(o); strings.Contains(r.Text, "'/bin/cc'") {
		t.Errorf("a safe path must stay unquoted:\n%s", r.Text)
	}
}

func TestSetRejectsKeysThatBreakOutOfTOML(t *testing.T) {
	for _, k := range []string{"a b", `a"b`, "a]b", "a\nb", "[table]", "", "é"} {
		for _, target := range []string{TargetCodex, TargetClaude} {
			o := base(t, target, "serving")
			o.Set = []string{k + "=v"}
			if _, err := Render(o); err == nil {
				t.Errorf("%s: --set key %q accepted", target, k)
			}
		}
	}
	o := base(t, TargetCodex, "serving")
	o.Set = []string{"model_reasoning_effort=high"}
	if _, err := Render(o); err != nil {
		t.Errorf("plain key refused: %v", err)
	}
}

func TestRescueScrubsInheritedAuthEnv(t *testing.T) {
	environ := []string{
		"CLAUDE_CODE_USE_BEDROCK=1", "CLAUDE_CODE_USE_VERTEX=1", "CLAUDE_CODE_OAUTH_TOKEN=x", "CLAUDE_CONFIG_DIR=/c",
		"DATABRICKS_TOKEN=x", "DATABRICKS_HOST=h", "DATABRICKS_CONFIG_PROFILE=p", "HARNESS_PROVIDER=p", "CLAUDE_PROFILE=dev", "CLAUDE_OPUS_MODEL=m", "ANTHROPIC_API_KEY=x",
		"PATH=/bin", "HOME=/h", "CLAUDE_CODE_NO_FLICKER=1", "MY_DATABRICKS_NOTE=keep",
	}
	got := scrubbed(environ)
	want := []string{"PATH=/bin", "HOME=/h", "CLAUDE_CODE_NO_FLICKER=1", "MY_DATABRICKS_NOTE=keep"}
	if !slices.Equal(got, want) {
		t.Errorf("scrubbed = %v, want %v", got, want)
	}
}
