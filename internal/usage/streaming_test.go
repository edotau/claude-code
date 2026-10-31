// usage_stream_test.go covers Run's concurrent section streaming: the MODEL table prints as soon as
// the local scan finishes, PROVIDER QUOTAS/API KEYS/AGENTS follow in order once their producers land,
// and a slow or disabled quota fetch never blocks or corrupts the other sections.
package usage

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuf is a mutex-guarded io.Writer so a test goroutine can read Run's output while Run (on its own
// goroutine) is still writing to it — bytes.Buffer alone would race under -race.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// waitUntil polls cond (each check synchronized via syncBuf) until true or the deadline, failing the
// test on timeout — no fixed sleep, so the test runs as fast as the streaming actually completes.
func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for condition")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestUsageStreamsSectionsInOrder: section 1 (MODEL) is fully written while the quota fetch is still
// blocked; releasing it then yields PROVIDER QUOTAS -> API KEYS -> AGENTS in that order, with the
// warn/exceeded trailer landing on errw only after Run finishes.
func TestUsageStreamsSectionsInOrder(t *testing.T) {
	claudeDir := t.TempDir()
	ts := time.Now().UTC().Format(time.RFC3339)
	writeUsageFixture(t, claudeDir, "proj", "sess", time.Minute, usageRecordLine("claude-opus-5", 100, 20, ts))

	release := make(chan struct{})
	stub := []quota{{Provider: "github", Account: "me", Plan: "Pro", Window: "monthly", UsedPct: 100, Used: 7000, Limit: 7000}}
	origQ := fetchQuotas
	fetchQuotas = func() []quota {
		<-release
		return stub
	}
	t.Cleanup(func() { fetchQuotas = origQ })

	var out, errw syncBuf
	done := make(chan int, 1)
	go func() { done <- Run([]string{"--days", "1"}, claudeDir, &out, &errw) }()

	waitUntil(t, 2*time.Second, func() bool { return strings.Contains(out.String(), "MODEL") })
	if strings.Contains(out.String(), "PROVIDER QUOTAS") {
		t.Fatalf("PROVIDER QUOTAS printed before the blocked quota fetch released:\n%s", out.String())
	}
	if errw.String() != "" {
		t.Fatalf("warnings printed before quotas were fetched: %q", errw.String())
	}
	close(release)

	var code int
	select {
	case code = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Run never returned after releasing the quota fetch")
	}
	if code != 0 {
		t.Fatalf("Run = %d; stderr:\n%s", code, errw.String())
	}

	s := out.String()
	idxModel := strings.Index(s, "MODEL")
	idxQuotas := strings.Index(s, "PROVIDER QUOTAS")
	idxKeys := strings.Index(s, "API KEYS")
	idxAgents := strings.Index(s, "AGENTS (last")
	if idxModel < 0 || idxQuotas < 0 || idxKeys < 0 || idxAgents < 0 {
		t.Fatalf("missing a section; MODEL@%d QUOTAS@%d KEYS@%d AGENTS@%d:\n%s", idxModel, idxQuotas, idxKeys, idxAgents, s)
	}
	if idxModel >= idxQuotas || idxQuotas >= idxKeys || idxKeys >= idxAgents {
		t.Fatalf("sections out of order: MODEL@%d QUOTAS@%d KEYS@%d AGENTS@%d:\n%s", idxModel, idxQuotas, idxKeys, idxAgents, s)
	}
	if !strings.Contains(errw.String(), "usage: github me at 100% (exceeded)") {
		t.Errorf("warn trailer missing from errw:\n%s", errw.String())
	}
}

// TestStartQuotaFetchDisabledNeverCallsFetch: enabled=false must short-circuit before ever touching the
// fetchQuotas seam — proven with a counting stub, not just a nil-result check.
func TestStartQuotaFetchDisabledNeverCallsFetch(t *testing.T) {
	called := false
	orig := fetchQuotas
	fetchQuotas = func() []quota { called = true; return []quota{{Provider: "should-not-appear"}} }
	t.Cleanup(func() { fetchQuotas = orig })

	ch := startQuotaFetch(false)
	got := <-ch
	if got != nil {
		t.Errorf("startQuotaFetch(false) result = %+v, want nil", got)
	}
	if called {
		t.Error("startQuotaFetch(false) must never call fetchQuotas")
	}
}

// TestStartQuotaFetchEnabledCallsFetch: the counting-stub complement — enabled=true does call it, and
// the returned quotas have LeftPct derived (100-UsedPct), proving setStates/LeftPct still run on this path.
func TestStartQuotaFetchEnabledCallsFetch(t *testing.T) {
	called := false
	orig := fetchQuotas
	fetchQuotas = func() []quota { called = true; return []quota{{Provider: "github", UsedPct: 30}} }
	t.Cleanup(func() { fetchQuotas = orig })

	ch := startQuotaFetch(true)
	got := <-ch
	if !called {
		t.Error("startQuotaFetch(true) must call fetchQuotas")
	}
	if len(got) != 1 || got[0].LeftPct != 70 || got[0].State != "ok" {
		t.Errorf("startQuotaFetch(true) result = %+v, want LeftPct=70 State=ok", got)
	}
}

// TestUsageJSONFixtureRegression: --format json stays one valid document with the pre-streaming key
// set and values intact — the JSON path gathers everything before encoding, so streaming must not leak
// a partial or duplicated document.
func TestUsageJSONFixtureRegression(t *testing.T) {
	claudeDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // the vault must be scratch, never the real one
	ts := time.Now().UTC().Format(time.RFC3339)
	writeUsageFixture(t, claudeDir, "proj", "sess", time.Minute, usageRecordLine("claude-opus-5", 100, 20, ts))

	origQ := fetchQuotas
	fetchQuotas = func() []quota {
		return []quota{{Provider: "github", Account: "me", Plan: "Pro+", Window: "monthly", UsedPct: 10, Used: 70, Limit: 700}}
	}
	t.Cleanup(func() { fetchQuotas = origQ })

	var out, errw bytes.Buffer
	if code := Run([]string{"--days", "1", "--format", "json"}, claudeDir, &out, &errw); code != 0 {
		t.Fatalf("exit %d; stderr:\n%s", code, errw.String())
	}

	// One valid document: Decode once, then confirm there is nothing else to decode.
	dec := json.NewDecoder(bytes.NewReader(out.Bytes()))
	var doc map[string]json.RawMessage
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode: %v\n%s", err, out.String())
	}
	if dec.More() {
		t.Fatalf("more than one JSON value in the output:\n%s", out.String())
	}

	wantKeys := []string{"models", "peak_input_tokens_per_min", "providers", "billing", "agents", "window_days"}
	for _, k := range wantKeys {
		if _, ok := doc[k]; !ok {
			t.Errorf("missing key %q in json doc:\n%s", k, out.String())
		}
	}
	if _, ok := doc["api"]; ok {
		t.Errorf(`"api" key should be omitted (omitempty) when the ledger is empty:\n%s`, out.String())
	}

	var models map[string]struct {
		Input  int64 `json:"input_tokens"`
		Output int64 `json:"output_tokens"`
	}
	if err := json.Unmarshal(doc["models"], &models); err != nil {
		t.Fatalf("unmarshal models: %v", err)
	}
	m, ok := models["claude-opus-5"]
	if !ok || m.Input != 100 || m.Output != 20 {
		t.Errorf("models[claude-opus-5] = %+v, want input=100 output=20", m)
	}

	var providers []quota
	if err := json.Unmarshal(doc["providers"], &providers); err != nil {
		t.Fatalf("unmarshal providers: %v", err)
	}
	if len(providers) != 1 || providers[0].Provider != "github" || providers[0].UsedPct != 10 {
		t.Errorf("providers = %+v, want one github row at 10%%", providers)
	}

	var windowDays int
	if err := json.Unmarshal(doc["window_days"], &windowDays); err != nil {
		t.Fatalf("unmarshal window_days: %v", err)
	}
	if windowDays != 1 {
		t.Errorf("window_days = %d, want 1", windowDays)
	}
}

// TestUsageAgentsClaudeRowMatchesModelTotals guards the agentsCh goroutine's closure: it must capture
// the SAME agg/files/days/session/claudeDir the caller already computed, not a later-mutated copy, so
// the AGENTS claude row's token total always equals the MODEL table's TOTAL column.
func TestUsageAgentsClaudeRowMatchesModelTotals(t *testing.T) {
	claudeDir := t.TempDir()
	ts := time.Now().UTC().Format(time.RFC3339)
	writeUsageFixture(t, claudeDir, "proj", "sess", time.Minute, usageRecordLine("claude-opus-5", 100, 20, ts))

	origQ := fetchQuotas
	fetchQuotas = func() []quota { return nil }
	t.Cleanup(func() { fetchQuotas = origQ })

	var out, errw bytes.Buffer
	if code := Run([]string{"--days", "1"}, claudeDir, &out, &errw); code != 0 {
		t.Fatalf("exit %d; stderr:\n%s", code, errw.String())
	}
	s := out.String()

	modelLine := grepLine(t, s, "claude-opus-5")
	fields := strings.Fields(modelLine)
	modelTotal := fields[len(fields)-1] // TOTAL is the last column

	agentsSection := s[strings.Index(s, "AGENTS (last"):]
	claudeTotalLine := grepLine(t, agentsSection, "claude")
	claudeFields := strings.Fields(claudeTotalLine)
	// AGENT PROVIDER/MODEL(total) TOKENS TURNS SESSIONS COST -> TOKENS is 3rd field after "(total)".
	claudeTokens := claudeFields[2]

	if modelTotal != "120" || claudeTokens != "120" {
		t.Fatalf("MODEL total=%q AGENTS claude tokens=%q, want both 120:\nMODEL line: %q\nAGENTS line: %q",
			modelTotal, claudeTokens, modelLine, claudeTotalLine)
	}
}

// grepLine returns the first line of s containing substr, failing the test if none matches.
func grepLine(t *testing.T, s, substr string) string {
	t.Helper()
	for line := range strings.SplitSeq(s, "\n") {
		if strings.Contains(line, substr) {
			return line
		}
	}
	t.Fatalf("no line containing %q in:\n%s", substr, s)
	return ""
}

// TestUsageSlowQuotaFetchLeavesAgentsIntactAndWarnsAfter: a slow (or failing-looking, i.e. Note-only)
// quota fetch must neither corrupt nor truncate the AGENTS section, and the warn/exceeded trailer on
// errw must not appear until the quota fetch itself has actually completed.
func TestUsageSlowQuotaFetchLeavesAgentsIntactAndWarnsAfter(t *testing.T) {
	claudeDir := t.TempDir()
	ts := time.Now().UTC().Format(time.RFC3339)
	writeUsageFixture(t, claudeDir, "proj", "sess", time.Minute, usageRecordLine("claude-opus-5", 100, 20, ts))

	release := make(chan struct{})
	origQ := fetchQuotas
	fetchQuotas = func() []quota {
		<-release                                                                   // simulates a slow provider round-trip
		return []quota{{Provider: "openai", Account: "me", UsedPct: 100, Note: ""}} // exceeded -> warns
	}
	t.Cleanup(func() { fetchQuotas = origQ })

	var out, errw syncBuf
	done := make(chan int, 1)
	go func() { done <- Run([]string{"--days", "1"}, claudeDir, &out, &errw) }()

	waitUntil(t, 2*time.Second, func() bool { return strings.Contains(out.String(), "MODEL") })
	if errw.String() != "" {
		t.Fatalf("warnings printed while the quota fetch was still blocked: %q", errw.String())
	}
	close(release)

	var code int
	select {
	case code = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Run never returned after releasing the slow quota fetch")
	}
	if code != 0 {
		t.Fatalf("Run = %d; stderr:\n%s", code, errw.String())
	}

	s := out.String()
	if !strings.Contains(s, "AGENTS (last") {
		t.Fatalf("AGENTS section missing after a slow quota fetch:\n%s", s)
	}
	agentsSection := s[strings.Index(s, "AGENTS (last"):]
	claudeLine := grepLine(t, agentsSection, "claude")
	if fields := strings.Fields(claudeLine); len(fields) < 3 || fields[2] != "120" {
		t.Errorf("AGENTS claude row corrupted by the slow fetch: %q", claudeLine)
	}
	if !strings.Contains(errw.String(), "usage: openai me at 100% (exceeded)") {
		t.Errorf("warn trailer missing from errw after the slow fetch released:\n%s", errw.String())
	}
}

// TestScanWorkflowNamesSurvivesHugeLine: a transcript line over 10MB must not drop the records after it.
func TestScanWorkflowNamesSurvivesHugeLine(t *testing.T) {
	use := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu1","input":{"name":"wfname"}}]}}`
	res := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","content":"Run ID: wf_abc123"}]}}`
	huge := `{"type":"x","pad":"` + strings.Repeat("a", 11<<20) + `"}`
	path := filepath.Join(t.TempDir(), "s.jsonl")
	writeFile(t, path, huge+"\n"+use+"\n"+res+"\n")
	if got := scanWorkflowNamesFromTranscript(path); got["wf_abc123"] != "wfname" {
		t.Fatalf("names = %v; want wf_abc123 -> wfname", got)
	}
}
