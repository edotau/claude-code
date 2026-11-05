package hooks

import (
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/memhook"
	"github.com/edotau/claude-code/internal/memory"
)

// memoryRunners are the memory-area Native entries and the memhook runner each must be.
var memoryRunners = map[string]core.Runner{
	"memory-start": memhook.Start, "memory-recall": memhook.Recall, "memory-update": memhook.Stop,
	"memory-end": memhook.End, "memory-compact": memhook.Compact,
}

// Every memory-* Native entry is the intended memhook runner, and no memory-* entry is missing from the map above.
func TestNativeMemoryEntriesAreMemhookRunners(t *testing.T) {
	for name, want := range memoryRunners {
		if reflect.ValueOf(Native[name]).Pointer() != reflect.ValueOf(want).Pointer() {
			t.Errorf("%s: Native entry is not the memhook runner", name)
		}
	}
	for name := range Native {
		if _, ok := memoryRunners[name]; strings.HasPrefix(name, "memory-") && !ok {
			t.Errorf("%s: memory-* Native entry has no expected memhook runner", name)
		}
	}
}

// SessionStart for a UUID session with a due dream reaches the stubbed spawns, so the go test binary is never relaunched.
func TestSessionStartMemorySpawnsAreStubbed(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cwd := t.TempDir()
	if _, err := memory.Init(cwd); err != nil {
		t.Fatal(err)
	}
	spawns := memhook.StubSpawnForTest(t, true)
	payload := `{"hook_event_name":"SessionStart","session_id":"44444444-4444-4444-8444-444444444444","cwd":"` + cwd + `"}`
	var out, errb strings.Builder
	if code := RunEvent("SessionStart", false, strings.NewReader(payload), &out, &errb); code != core.ExitProceed {
		t.Fatalf("SessionStart exit %d, stderr %q", code, errb.String())
	}
	if spawns.Dream.Load() != 1 || spawns.Catchup.Load() != 1 {
		t.Errorf("dream=%d catchup=%d spawn-stub hits, want 1 each", spawns.Dream.Load(), spawns.Catchup.Load())
	}
}
