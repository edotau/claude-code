package hooks

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/docs"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// plainDocsRunners are the docs-area native entries registered without a viaStderr wrapper.
var plainDocsRunners = map[string]core.Runner{
	"docs-plan-skeleton": docs.Skeleton, "docs-plan-skeleton-prompt": docs.Skeleton, "docs-plan-tick": docs.Tick,
	"docs-plan-reminder": docs.Reminder, "paths-resolve": docs.ResolvePaths,
}

// stderrDocsRunners are the docs-area helpers native wraps in viaStderr.
var stderrDocsRunners = map[string]func(*core.Input, io.Writer) int{
	"docs-placement": docs.Placement, "docs-plan-gate": docs.Gate, "docs-plan-mirror": docs.Mirror, "docs-plan-save": docs.Save,
}

// Every docs-area native entry is the intended docs runner: same function, or the same Result as the wrapped helper.
func TestNativeDocsEntriesAreDocsRunners(t *testing.T) {
	for name, want := range plainDocsRunners {
		if reflect.ValueOf(native[name]).Pointer() != reflect.ValueOf(want).Pointer() {
			t.Errorf("%s: native entry is not the docs runner", name)
		}
	}
	for name, helper := range stderrDocsRunners {
		payload := `{"tool_name":"Write","tool_input":{"file_path":"` + filepath.Join(t.TempDir(), "x.txt") + `"}}`
		var b bytes.Buffer
		want := core.ExitResult(helper(core.ParseInput(strings.NewReader(payload)), &b), b.String())
		if got := native[name](core.ParseInput(strings.NewReader(payload))); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: native result %+v, want %+v", name, got, want)
		}
	}
}

// The paths and codex groups run natively: Bind must hand each declared handler the Runner from
// native (same function pointer), and that Runner returns a Result directly.
func TestBindPrefersNativeRunners(t *testing.T) {
	if len(native) == 0 {
		t.Fatal("native is empty")
	}
	wantNativeBindings(t)
}

// wantNativeBindings requires every native runner to be bound by Bind to the same function pointer.
func wantNativeBindings(t *testing.T) {
	t.Helper()
	seen := map[string]bool{}
	for _, b := range Bind() {
		want, ok := native[b.Spec.Name]
		if !ok {
			continue
		}
		seen[b.Spec.Name] = true
		if reflect.ValueOf(b.Run).Pointer() != reflect.ValueOf(want).Pointer() {
			t.Errorf("%s: not bound to its native runner", b.Spec.Name)
		}
	}
	for name := range native {
		if !seen[name] {
			t.Errorf("%s: declared in native but never bound", name)
		}
	}
}

// wantVendorPromptDoc checks the vendor-specific shape of the paths-resolve JSON document.
func wantVendorPromptDoc(t *testing.T, vendor string, doc map[string]any) {
	t.Helper()
	switch vendor {
	case core.AgentClaude:
		h, _ := doc["hookSpecificOutput"].(map[string]any)
		sys, _ := doc["systemMessage"].(string)
		ctx, _ := h["additionalContext"].(string)
		if sys == "" || ctx == "" {
			t.Errorf("claude doc = %v, want systemMessage + hookSpecificOutput.additionalContext", doc)
		}
	case core.AgentCopilot:
		ctx, _ := doc["additionalContext"].(string)
		if ctx == "" || len(doc) != 1 {
			t.Errorf("copilot doc = %v, want only additionalContext", doc)
		}
	}
}

// A direct `hook UserPromptSubmit --paths-resolve` call and the UserPromptSubmit dispatcher emit the same document.
func TestPathResolverRunMatchesDispatcher(t *testing.T) {
	for _, vendor := range []string{core.AgentClaude, core.AgentCopilot} {
		t.Run(vendor, func(t *testing.T) {
			vaulttest.Use(t) // hermetic CLAUDE_CONFIG_DIR + vault (harness hook switches)
			t.Setenv(settings.AgentEnv, vendor)
			t.Setenv("CLAUDE_RECALL_SEMANTIC", "0") // keep the co-dispatched memory-recall cheap
			dir := t.TempDir()
			f := filepath.Join(dir, "notes.md")
			if err := os.WriteFile(f, []byte("hello\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			payload := `{"prompt":"read ` + f + `"}`

			var directOut, directErr bytes.Buffer
			if code := RunArgs("UserPromptSubmit", []string{"--paths-resolve"}, strings.NewReader(payload), &directOut, &directErr); code != core.ExitProceed {
				t.Fatalf("Run exit = %d (stderr %s)", code, &directErr)
			}
			var eventOut, eventErr bytes.Buffer
			if code := runEvent("UserPromptSubmit", false, strings.NewReader(payload), &eventOut, &eventErr); code != core.ExitProceed {
				t.Fatalf("runEvent exit = %d (stderr %s)", code, &eventErr)
			}
			if directOut.String() != eventOut.String() {
				t.Fatalf("direct %q != dispatched %q", &directOut, &eventOut)
			}
			wantVendorPromptDoc(t, vendor, hosttest.OneDoc(t, directOut.String()))
		})
	}
}
