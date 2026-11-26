package dispatchhook

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
)

const smBody = `"tool_input":{"subagent_type":"general-purpose","prompt":"fix the typo in the help text","description":"fix typo"}`

func smPayload(mode, session string) string {
	return `{"harness_spawn":"` + mode + `","session_id":"` + session + `","permission_mode":"default","tool_name":"Agent",` + smBody + `}`
}

func smRun(payload string) core.Result {
	return EffortSpawn(core.ParseInput(strings.NewReader(payload)))
}

func smHome(t *testing.T) {
	t.Helper()
	effortHome(t)
	stubLabelModels(t, "")
}

func smTree(t *testing.T) map[string]time.Time {
	t.Helper()
	out := map[string]time.Time{}
	root := os.Getenv("CLAUDE_CONFIG_DIR")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			st, _ := d.Info()
			out[p] = st.ModTime()
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Fails without the marker code: the ready call would not print {} and PreToolUse would keep its updatedInput.
func TestSpawnMarkerMovesRewritesToNotes(t *testing.T) {
	smHome(t)
	today := smRun(smPayload("", "s-today"))
	if today.UpdatedInput == nil {
		t.Fatalf("no-marker PreToolUse = %+v, want today's updatedInput", today)
	}
	if r := smRun(`{"harness_spawn":"ready","session_id":"s-ready"}`); r.Stdout != "{}" {
		t.Fatalf("ready stdout = %q, want {}", r.Stdout)
	}
	r := smRun(smPayload("", "s-ready"))
	if r.UpdatedInput != nil || r.SystemMessage != "" || strings.Contains(r.Context, "model=haiku") {
		t.Fatalf("marked PreToolUse = %+v, want no pin or label (the mod's query makes them)", r)
	}
	if r2 := smRun(smPayload("", "s-other")); r2.UpdatedInput == nil {
		t.Fatalf("another session = %+v, want today's updatedInput", r2)
	}
}

// Fails without the query: the call falls through to PreToolUse and prints no spawn object.
func TestSpawnQueryAnswersChangedFieldsOnly(t *testing.T) {
	smHome(t)
	var got map[string]any
	if err := json.Unmarshal([]byte(smRun(smPayload("query", "s-q")).Stdout), &got); err != nil {
		t.Fatal(err)
	}
	if got["description"] != "haiku: fix typo" || got["model"] != "haiku" || got["prompt"] != nil || got["subagentType"] != nil {
		t.Fatalf("query = %v, want description and model only", got)
	}
}

// Fails if the query claimed, recalled or stamped: any file under the state dir would appear or change.
func TestSpawnQueryStampsNothing(t *testing.T) {
	smHome(t)
	seedHomeRecallBlock(t, 8)
	before := smTree(t)
	p := strings.Replace(smPayload("query", "s-stamp"), "fix the typo in the help text", recallPrompt, 1)
	if r := smRun(p); !strings.HasPrefix(r.Stdout, "{") {
		t.Fatalf("query stdout = %q", r.Stdout)
	}
	after := smTree(t)
	if len(after) != len(before) {
		t.Fatalf("query wrote files: before %d after %d", len(before), len(after))
	}
	for f, m := range before {
		if !after[f].Equal(m) {
			t.Errorf("query changed %s", f)
		}
	}
}

// Fails if the age check is missing: a marker past seven days would still defer.
func TestSpawnMarkerExpires(t *testing.T) {
	smHome(t)
	smRun(`{"harness_spawn":"ready","session_id":"s-old"}`)
	old := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(core.SeenFile(readyKind, "s-old"), old, old); err != nil {
		t.Fatal(err)
	}
	if r := smRun(smPayload("", "s-old")); r.UpdatedInput == nil {
		t.Fatalf("stale marker = %+v, want today's updatedInput", r)
	}
}

// Fails without the retype carve-out: a marked stale-type call would get notes only and the host would reject the type.
func TestSpawnMarkerKeepsStaleRetype(t *testing.T) {
	smHome(t)
	call := func(session string) core.Result {
		return smRun(strings.Replace(smPayload("", session), `"general-purpose"`, `"code-mode"`, 1))
	}
	smRun(`{"harness_spawn":"ready","session_id":"s-co"}`)
	marked, today := call("s-co"), call("s-co-none")
	if marked.UpdatedInput["subagent_type"] != "coworker" || marked.UpdatedInput["subagent_type"] != today.UpdatedInput["subagent_type"] {
		t.Fatalf("marked = %+v, today = %+v, want updatedInput retyped to coworker", marked, today)
	}
}
