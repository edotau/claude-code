package hooks

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/redact"

	_ "github.com/edotau/claude-code/internal/nonet"
)

const fakeSecret = "ccfake-0123456789abcdef0123456789abcdef"

func bashOutput(stdout, stderr string) *core.Input {
	return core.ParseInput(strings.NewReader(`{"tool_name":"Bash","hook_event_name":"PostToolUse","tool_response":{"stdout":` +
		quote(stdout) + `,"stderr":` + quote(stderr) + `,"interrupted":false,"isImage":false,"noOutputExpected":false}}`))
}

func quote(s string) string { return `"` + s + `"` }

// A vault value in stdout or stderr is masked at the same length, other fields stay, other calls return nothing.
func TestSecretsOutMasksVaultValuesOnly(t *testing.T) {
	redact.StubValues(t, []string{fakeSecret})
	got := SecretsOut(bashOutput("token="+fakeSecret+" ok", "err "+fakeSecret))
	if got.UpdatedToolOutput == nil {
		t.Fatal("a vault value in the output was not replaced")
	}
	stdout, _ := got.UpdatedToolOutput["stdout"].(string)
	stderr, _ := got.UpdatedToolOutput["stderr"].(string)
	if len(stdout) != len("token="+fakeSecret+" ok") || strings.Contains(stdout+stderr, fakeSecret) || !strings.HasSuffix(stdout, " ok") {
		t.Errorf("stdout %q stderr %q: want same length, value gone, rest intact", stdout, stderr)
	}
	if got.UpdatedToolOutput["interrupted"] != false || len(got.UpdatedToolOutput) != 5 {
		t.Errorf("other response fields changed: %v", got.UpdatedToolOutput)
	}
	var out bytes.Buffer
	core.Emit(core.AgentClaude, "PostToolUse", got, &out, &out)
	if !strings.Contains(out.String(), `"updatedToolOutput"`) || strings.Contains(out.String(), fakeSecret) {
		t.Errorf("rendered %q, want updatedToolOutput without the value", out.String())
	}
	for name, in := range map[string]*core.Input{
		"no value":     bashOutput("hello world, nothing secret here", ""),
		"shape only":   bashOutput("key sk-learn-something-long-enough-0123456789", ""),
		"short output": bashOutput("ok", ""),
		"no response":  core.ParseInput(strings.NewReader(`{"tool_name":"Bash"}`)),
	} {
		if r := SecretsOut(in); r.UpdatedToolOutput != nil || r.Stderr != "" || r.Context != "" {
			t.Errorf("%s: returned %+v, want nothing", name, r)
		}
	}
}

// Only the string stream that held a value changes: a non-string stream keeps its type, an absent one stays absent.
func TestSecretsOutKeepsResponseShape(t *testing.T) {
	redact.StubValues(t, []string{fakeSecret})
	for name, stderr := range map[string]string{"object": `,"stderr":{"isImage":true}`, "array": `,"stderr":[1]`,
		"null": `,"stderr":null`, "absent": ``} {
		in := core.ParseInput(strings.NewReader(`{"tool_name":"Bash","tool_response":{"stdout":"t=` + fakeSecret + `"` + stderr + `,"n":3}}`))
		got := SecretsOut(in).UpdatedToolOutput
		orig := in.Field("tool_response").(map[string]any)
		if got == nil || strings.Contains(got["stdout"].(string), fakeSecret) {
			t.Fatalf("%s: stdout not masked: %v", name, got)
		}
		if len(got) != len(orig) || fmt.Sprint(got["stderr"]) != fmt.Sprint(orig["stderr"]) || got["n"] != orig["n"] {
			t.Errorf("%s: response %v, want %v with only stdout masked", name, got, orig)
		}
	}
}
