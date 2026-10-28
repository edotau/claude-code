package router

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAccountKeysUseProviderDotProfile(t *testing.T) {
	tb := testTable(t)
	want := map[string]string{"codex": "openai·chatgpt", "gemini": "google·antigravity", "copilot": "github·work-user"}
	for agent, acct := range want {
		if got := tb.Rows[agent]["sonnet"].Account; got != acct {
			t.Errorf("%s account = %q, want %q", agent, got, acct)
		}
	}
	if got := ProviderAccount("no-such-profile"); got != "databricks·no-such-profile" {
		t.Errorf("unregistered profile = %q, want databricks fallback", got)
	}
}

func TestCandidateAndSkipJSONUseAgentField(t *testing.T) {
	out, err := json.Marshal(Result{
		Candidates: []Candidate{{Agent: "codex"}},
		Skipped:    []Skip{{Agent: "copilot", Gate: GateExhausted, Reason: "r"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Count(s, `"agent":`) != 2 || strings.Contains(s, `"runner"`) {
		t.Errorf("JSON = %s, want two agent fields and no runner", s)
	}
}

// Negative control: marks under the retired "<agent>:<profile>" keys no longer gate or match.
func TestOldColonKeysNoLongerMatch(t *testing.T) {
	tb := testTable(t)
	old := State{"codex:chatgpt": {AuthReason: "auth rejected", AuthAt: now0}}
	if !strings.Contains(runners(pick(tb, "sonnet", "codex", old, FallbackVendors)), "codex") {
		t.Error("an old-format auth mark still gated codex")
	}
	cur := State{"openai·chatgpt": {AuthReason: "auth rejected", AuthAt: now0}}
	if strings.Contains(runners(pick(tb, "sonnet", "codex", cur, FallbackVendors)), "codex") {
		t.Error("the new-format auth mark did not gate codex")
	}
	if _, ok := old.accountAuthMarked("openai·chatgpt", now0); ok {
		t.Error("old key matched the new account")
	}
}
