package hooks

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// hermeticRouting points CLAUDE_CONFIG_DIR at a fresh temp dir so decisions.jsonl/audit.jsonl never
// touch the real repo state.
func hermeticRouting(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
}

func runAgentRouted(t *testing.T, stdin string) int {
	t.Helper()
	return AgentRouted(strings.NewReader(stdin))
}

// TestAgentRoutedReconcilesOldestUnmatchedDecision: two decisions logged for the same session; the
// first agent-routed call claims the OLDEST one, not the most recent.
func TestAgentRoutedReconcilesOldestUnmatchedDecision(t *testing.T) {
	hermeticRouting(t)
	appendDecision(map[string]any{"decision_id": "d-old", "session_id": "s1", "tier": "low", "signal": "mechanical", "model": "haiku", "at": 1})
	appendDecision(map[string]any{"decision_id": "d-new", "session_id": "s1", "tier": "medium", "signal": "neutral", "model": "sonnet", "at": 2})

	if code := runAgentRouted(t, `{"session_id":"s1","agent_id":"a1"}`); code != ExitProceed {
		t.Fatalf("exit = %d, want ExitProceed", code)
	}
	recs := readJSONLRecords(auditPath())
	if len(recs) != 1 {
		t.Fatalf("audit rows = %d, want 1: %v", len(recs), recs)
	}
	if recs[0]["decision_id"] != "d-old" {
		t.Errorf("reconciled decision_id = %v, want d-old (the oldest unmatched)", recs[0]["decision_id"])
	}
}

// TestAgentRoutedPrefersModelMatchOverOldest: when the actual launch model names a non-oldest
// pending decision, that decision is preferred over the merely-oldest one.
func TestAgentRoutedPrefersModelMatchOverOldest(t *testing.T) {
	hermeticRouting(t)
	appendDecision(map[string]any{"decision_id": "d-old", "session_id": "s1", "model": "haiku", "at": 1})
	appendDecision(map[string]any{"decision_id": "d-new", "session_id": "s1", "model": "sonnet", "at": 2})

	runAgentRouted(t, `{"session_id":"s1","agent_id":"a1","model":"sonnet"}`)
	recs := readJSONLRecords(auditPath())
	if len(recs) != 1 || recs[0]["decision_id"] != "d-new" {
		t.Fatalf("audit = %v, want the sonnet-model decision d-new preferred over the oldest", recs)
	}
	if recs[0]["matches"] != true {
		t.Errorf("matches = %v, want true (model equals the reconciled decision's model)", recs[0]["matches"])
	}
}

// TestAgentRoutedMatchesNullWithoutModel: a SubagentStart payload carrying no model at all records
// matches: null, not false — false would wrongly claim a comparison was made.
func TestAgentRoutedMatchesNullWithoutModel(t *testing.T) {
	hermeticRouting(t)
	appendDecision(map[string]any{"decision_id": "d1", "session_id": "s1", "model": "haiku", "at": 1})

	runAgentRouted(t, `{"session_id":"s1","agent_id":"a1"}`)
	recs := readJSONLRecords(auditPath())
	if len(recs) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(recs))
	}
	raw, err := os.ReadFile(auditPath())
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(strings.Split(string(raw), "\n")[0]), &doc); err != nil {
		t.Fatal(err)
	}
	if v, ok := doc["matches"]; !ok || v != nil {
		t.Errorf("matches = %v (present=%v), want present and null", v, ok)
	}
}

// TestAgentRoutedNeverReusesADecisionID: a second SubagentStart for the same session, after the
// first already claimed the only decision, must not reconcile against it again.
func TestAgentRoutedNeverReusesADecisionID(t *testing.T) {
	hermeticRouting(t)
	appendDecision(map[string]any{"decision_id": "d1", "session_id": "s1", "model": "haiku", "at": 1})

	runAgentRouted(t, `{"session_id":"s1","agent_id":"a1"}`)
	runAgentRouted(t, `{"session_id":"s1","agent_id":"a2"}`)

	recs := readJSONLRecords(auditPath())
	if len(recs) != 2 {
		t.Fatalf("audit rows = %d, want 2", len(recs))
	}
	if recs[0]["decision_id"] != "d1" {
		t.Fatalf("first row decision_id = %v, want d1", recs[0]["decision_id"])
	}
	if recs[1]["decision_id"] != nil && recs[1]["decision_id"] != "" {
		t.Errorf("second row must find no pending decision left to claim, got decision_id=%v", recs[1]["decision_id"])
	}
}

// TestAgentRoutedUnwritableDirStillProceeds: CLAUDE_CONFIG_DIR pointed at a file (not a directory)
// makes every state path under it fail to mkdir — the hook must still exit ExitProceed.
func TestAgentRoutedUnwritableDirStillProceeds(t *testing.T) {
	home := t.TempDir()
	notADir := home + "/blocked"
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", notADir)
	if code := runAgentRouted(t, `{"session_id":"s1","agent_id":"a1"}`); code != ExitProceed {
		t.Errorf("exit = %d, want ExitProceed despite an unwritable routing dir", code)
	}
}

// TestRotateIfOversizeRotatesPastOneMiB: decisions.jsonl past 1 MiB rotates to a single .1 generation,
// which is overwritten (not appended to) on a later rotation.
func TestRotateIfOversizeRotatesPastOneMiB(t *testing.T) {
	hermeticRouting(t)
	if err := os.MkdirAll(routingDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, decisionsCap+1)
	for i := range big {
		big[i] = 'x'
	}
	if err := os.WriteFile(decisionsPath(), big, 0o600); err != nil {
		t.Fatal(err)
	}
	rotateIfOversize(decisionsPath())
	if _, err := os.Stat(decisionsPath() + ".1"); err != nil {
		t.Fatalf("expected a .1 rotation file: %v", err)
	}
	if _, err := os.Stat(decisionsPath()); !os.IsNotExist(err) {
		t.Errorf("decisions.jsonl must be renamed away, not copied, stat err = %v", err)
	}

	// A second rotation overwrites .1 rather than appending another generation.
	if err := os.WriteFile(decisionsPath(), big, 0o600); err != nil {
		t.Fatal(err)
	}
	rotateIfOversize(decisionsPath())
	if _, err := os.Stat(decisionsPath() + ".2"); !os.IsNotExist(err) {
		t.Errorf("only one prior generation (.1) must ever exist, found a .2")
	}
}
