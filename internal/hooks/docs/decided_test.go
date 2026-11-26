package docs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/plans"
)

// A recorded DECISION with a strike token strikes that exact task once, citing it; words alone strike nothing.
func TestStrikeDecidedExactTaskOnce(t *testing.T) {
	_, home := planGroupEnv(t, false)
	repo := filepath.Join(home, "widgets")
	hosttest.In(t, filepath.Dir(repo), "init", "-q", "-b", "main", repo)
	slug := time.Now().Format(time.DateOnly) + "-widgets"
	plan := filepath.Join(plans.Home(repo), slug+".md")
	body := "- [ ] 1.1 Keep owns: a.go\n- [ ] 1.2 Drop owns: b.go\n- [x] 1.3 Done owns: c.go (abc1234)\n"
	hosttest.WriteRel(t, filepath.Dir(plan), filepath.Base(plan), body)
	session := "decided1"
	writeTagEvents(t, session,
		"we keep 1.1 and stop 1.2",                     // names ids in words: no token, so nothing
		"auth moves to the proxy; strikes: "+slug+" 1", // "1" is no task's exact id
		"the proxy owns it now; strikes: "+slug+" 1.2 (superseded)",
		"strikes: "+slug+" 1.3")
	tick := func() string {
		return runDocs(t, Tick, map[string]any{"hook_event_name": "PostToolUse", "session_id": session, "tool_name": "Bash",
			"cwd": repo, "tool_input": map[string]string{"command": "ls"}, "tool_response": map[string]string{"stdout": ""}})
	}
	note := tick()
	got := readPlanFile(t, plan)
	if !strings.Contains(got, "- [ ] 1.1 Keep owns: a.go\n") || !strings.Contains(got, "~~1.2 Drop owns: b.go~~") ||
		!strings.Contains(got, "— superseded: superseded — evidence: note:"+memory.TagEventsPath(session)) ||
		!strings.Contains(got, "[x] 1.3 Done") {
		t.Fatalf("plan after the decisions:\n%s", got)
	}
	for _, want := range []string{"plans: struck 1.2 in " + slug, "strikes " + slug + " 1: 0 tasks", "1.3: plans: task \"1.3\" is ticked"} {
		if !strings.Contains(note, want) {
			t.Errorf("missing %q in:\n%s", want, note)
		}
	}
	if again := tick(); again != "" || readPlanFile(t, plan) != got {
		t.Fatalf("a second run must strike and say nothing: %q", again)
	}
}

// writeTagEvents records texts as the session's DECISION events, as the Stop hook does.
func writeTagEvents(t *testing.T, session string, texts ...string) {
	t.Helper()
	var b strings.Builder
	for _, text := range texts {
		line, err := json.Marshal(memory.TagEvent{Event: "decision", Text: text, At: time.Now().UTC().Format(time.RFC3339)})
		if err != nil {
			t.Fatal(err)
		}
		b.Write(append(line, '\n'))
	}
	path := memory.TagEventsPath(session)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readPlanFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
