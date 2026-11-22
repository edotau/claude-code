package hooks

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/codex"
	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/memhook"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/memory/memtest"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Every command in the rendered codex/copilot hook files is `.hookd <Event> --<module>` for a module on that event.
func TestVendorHookFilesCallOnlyResolvableHooks(t *testing.T) {
	cmdRe := regexp.MustCompile(`(?:claude-code hook|\.hookd) (\S+)(?: --([a-z][a-z0-9-]*))?`)
	for name, render := range map[string]func() ([]byte, error){"codex": codex.CodexJSON, "copilot": copilot.CopilotJSON} {
		body, err := render()
		if err != nil {
			t.Fatal(err)
		}
		ms := cmdRe.FindAllStringSubmatch(string(body), -1)
		if len(ms) == 0 {
			t.Fatalf("%s: no hook commands rendered", name)
		}
		for _, m := range ms {
			if _, ok := native[m[2]]; !ok || !slices.Contains(settings.HookEvents(), m[1]) || eventOf(m[2]) != m[1] {
				t.Errorf("%s hook file calls %q, which no module on that event serves", name, m[0])
			}
		}
	}
}

// A direct `hook UserPromptSubmit --paths-resolve` call and the UserPromptSubmit dispatcher emit the same document.
func TestPathResolverRunMatchesDispatcher(t *testing.T) {
	for _, vendor := range []string{core.AgentClaude, core.AgentCopilot} {
		t.Run(vendor, func(t *testing.T) {
			vaulttest.Use(t) // hermetic CLAUDE_CONFIG_DIR + vault (harness hook switches)
			t.Setenv(settings.AgentEnv, vendor)
			t.Setenv("CLAUDE_RECALL_SEMANTIC", "0")     // keep the co-dispatched memory-recall cheap
			f := filepath.Join(t.TempDir(), "shot.png") // an image path still reaches the model; a plain .md does not
			if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			payload := `{"prompt":"read ` + f + `"}`

			var directOut, directErr bytes.Buffer
			if code := RunArgs("UserPromptSubmit", []string{"--paths-resolve"}, strings.NewReader(payload), &directOut, &directErr); code != core.ExitProceed {
				t.Fatalf("Run exit = %d (stderr %s)", code, &directErr)
			}
			var eventOut, eventErr bytes.Buffer
			if code := runEvent("UserPromptSubmit", "", strings.NewReader(payload), &eventOut, &eventErr); code != core.ExitProceed {
				t.Fatalf("runEvent exit = %d (stderr %s)", code, &eventErr)
			}
			if directOut.String() != eventOut.String() {
				t.Fatalf("direct %q != dispatched %q", &directOut, &eventOut)
			}
			wantVendorPromptDoc(t, vendor, hosttest.OneDoc(t, directOut.String()))
		})
	}
}

// memoryRunners are the memory-area native entries and the memhook runner each must be.
var memoryRunners = map[string]core.Runner{
	"memory-start": WithDispatchLabels(memhook.Start), "memory-recall": WithDispatchLabels(memhook.Recall), "memory-update": memhook.Stop,
	"memory-end": memhook.End, "memory-compact": memhook.Compact,
}

// Every memory-* native entry is the intended memhook runner, and no memory-* entry is missing from the map above.
func TestNativeMemoryEntriesAreMemhookRunners(t *testing.T) {
	for name, want := range memoryRunners {
		if reflect.ValueOf(native[name]).Pointer() != reflect.ValueOf(want).Pointer() {
			t.Errorf("%s: native entry is not the memhook runner", name)
		}
	}
	for name := range native {
		if _, ok := memoryRunners[name]; strings.HasPrefix(name, "memory-") && !ok {
			t.Errorf("%s: memory-* native entry has no expected memhook runner", name)
		}
	}
}

// A due dream at SessionStart queues and wakes the stubbed sweep, never relaunching the test binary.
func TestSessionStartMemorySpawnsAreStubbed(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cwd := t.TempDir()
	if _, err := memory.Init(); err != nil {
		t.Fatal(err)
	}
	spawns := memtest.StubSpawn(t, true)
	payload := `{"hook_event_name":"SessionStart","session_id":"44444444-4444-4444-8444-444444444444","cwd":"` + cwd + `"}`
	var out, errb strings.Builder
	if code := runEvent("SessionStart", "", strings.NewReader(payload), &out, &errb); code != core.ExitProceed {
		t.Fatalf("SessionStart exit %d, stderr %q", code, errb.String())
	}
	queued, _ := filepath.Glob(filepath.Join(paths.MemoryStateDir(), "queue", "dream-*.json"))
	if spawns.Catchup.Load() != 1 || len(queued) != 1 {
		t.Errorf("catchup=%d queued dreams=%d, want 1 each", spawns.Catchup.Load(), len(queued))
	}
}
