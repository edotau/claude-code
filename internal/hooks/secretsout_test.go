package hooks

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/redact"
	"github.com/edotau/claude-code/internal/vault"

	_ "github.com/edotau/claude-code/internal/nonet"
)

const fakeSecret = "ccfake-0123456789abcdef0123456789abcdef"

func bashOutput(stdout, stderr string) *core.Input {
	return core.ParseInput(strings.NewReader(`{"tool_name":"Bash","hook_event_name":"PostToolUse","tool_response":{"stdout":` +
		quote(stdout) + `,"stderr":` + quote(stderr) + `,"interrupted":false,"isImage":false,"noOutputExpected":false}}`))
}

func quote(s string) string { return `"` + s + `"` }

// A vault value in stdout or stderr is masked at the same length, other fields stay, other calls return nothing.
func TestSecretsOutMasksVaultValues(t *testing.T) {
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
}

// Output with no vault value, too short to open, or with no response returns no replacement.
func TestSecretsOutLeavesCleanOutput(t *testing.T) {
	redact.StubValues(t, []string{fakeSecret})
	for name, in := range map[string]*core.Input{
		"no value":     bashOutput("hello world, nothing secret here", ""),
		"short output": bashOutput("ok", ""),
		"no response":  core.ParseInput(strings.NewReader(`{"tool_name":"Bash"}`)),
	} {
		if r := SecretsOut(in); r.UpdatedToolOutput != nil || r.Stderr != "" || r.Context != "" {
			t.Errorf("%s: returned %+v, want nothing", name, r)
		}
	}
}

// A credential shape (ghp_, tskey-) is masked at the same length with the vault loaded, not only when it is unreadable.
func TestSecretsOutMasksShapesWithVault(t *testing.T) {
	redact.StubValues(t, []string{fakeSecret})
	ghp, tskey := "gh"+"p_"+strings.Repeat("Ab1", 12), "tsk"+"ey-auth-kTEST000CNTRL-"+strings.Repeat("abcde", 6)
	shapes := SecretsOut(bashOutput("a "+ghp+" b", "c "+tskey+" d")).UpdatedToolOutput
	if o, e := shapes["stdout"].(string), shapes["stderr"].(string); len(o) != len("a "+ghp+" b") || len(e) != len("c "+tskey+" d") ||
		strings.Contains(o, ghp) || strings.Contains(e, tskey) || !strings.HasSuffix(e, " d") {
		t.Errorf("shapes with the vault loaded: stdout %q stderr %q, want both masked at the same length", o, e)
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

// A vault read that stalls past the wait still masks credential shapes, with a note, before the hook budget ends.
func TestSecretsOutStalledVaultMasksShapes(t *testing.T) {
	release := make(chan struct{})
	redact.StubSource(t, func() ([]string, error) { <-release; return []string{fakeSecret}, nil })
	t.Cleanup(func() { close(release) })
	prev := vaultWait
	vaultWait = func() time.Duration { return 20 * time.Millisecond }
	t.Cleanup(func() { vaultWait = prev })
	shape := "gh" + "p_" + strings.Repeat("Zq8r", 9)
	got := SecretsOut(bashOutput("GH="+shape+" ok", ""))
	stdout, _ := got.UpdatedToolOutput["stdout"].(string)
	if strings.Contains(stdout, shape) || len(stdout) != len("GH="+shape+" ok") || got.Stderr != shapesOnlyNote {
		t.Errorf("stdout %d bytes masked=%v, stderr %q", len(stdout), !strings.Contains(stdout, shape), got.Stderr)
	}
}

// An unreadable vault sends the message on the call that sets the latch, not on the next one inside the minute.
func TestSecretsOutFallbackMessageOncePerMinute(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	redact.StubSource(t, func() ([]string, error) { return nil, vault.ErrLocked })
	first := SecretsOut(bashOutput("plain output long enough to open", ""))
	if !strings.Contains(first.SystemMessage, "could not be loaded (no key or vault locked)") || first.Stderr != shapesOnlyNote {
		t.Errorf("first call: message %q stderr %q", first.SystemMessage, first.Stderr)
	}
	if next := SecretsOut(bashOutput("plain output long enough to open", "")); next.SystemMessage != "" || next.Stderr != shapesOnlyNote {
		t.Errorf("next call: message %q stderr %q, want no message and the stderr note", next.SystemMessage, next.Stderr)
	}
}
