package usage

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/tokenledger"
	"github.com/edotau/claude-code/internal/transcript"
)

// writeFile creates path (with parents) holding content at 0o600.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// writeUsageFixture writes one project/session/subagents/agent-*.jsonl tree under claudeDir and
// backdates the transcript and directories — usageFilesWithinDays uses the newest mtime.
func writeUsageFixture(t *testing.T, claudeDir, project, session string, age time.Duration, lines ...string) string {
	t.Helper()
	p := filepath.Join(claudeDir, "projects", project, session, "subagents", "agent-a.jsonl")
	writeFile(t, p, strings.Join(lines, "\n"))
	mtime := time.Now().Add(-age)
	sessionDir := filepath.Join(claudeDir, "projects", project, session)
	for _, path := range []string{p, filepath.Dir(p), sessionDir} {
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// recentTS is an RFC3339 timestamp age before now, so a --days window keeps the record.
func recentTS(age time.Duration) string {
	return time.Now().Add(-age).UTC().Format("2006-01-02T15:04:05.000Z")
}

func usageRecordLine(model string, input, output int, ts string) string {
	return `{"type":"assistant","timestamp":"` + ts + `","message":{"model":"` + model +
		`","usage":{"input_tokens":` + strconv.Itoa(input) + `,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":` + strconv.Itoa(output) + `}}}`
}

// readTestdata returns the real-byte fixture content at internal/usage/testdata/<name>.
func readTestdata(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read testdata %s: %v", name, err)
	}
	return string(raw)
}

func TestUsageCmdDaysScope(t *testing.T) {
	claudeDir := t.TempDir()
	writeUsageFixture(t, claudeDir, "-home-tester-repoA", "session-recent", time.Hour,
		usageRecordLine("claude-sonnet-5", 100, 10, recentTS(time.Hour)))
	writeUsageFixture(t, claudeDir, "-home-tester-repoB", "session-old", 30*24*time.Hour,
		usageRecordLine("claude-opus-4-8", 9000, 900, "2026-06-01T00:00:00.000Z"))

	var out, errw bytes.Buffer
	if code := Run([]string{"--days", "1"}, claudeDir, &out, &errw); code != 0 {
		t.Fatalf("Run = %d, want 0; stderr:\n%s", code, errw.String())
	}
	got := out.String()
	if !strings.Contains(got, "claude-sonnet-5") {
		t.Errorf("table missing recent model:\n%s", got)
	}
	if strings.Contains(got, "claude-opus-4-8") {
		t.Errorf("table should NOT include the 30-day-old session outside --days 1:\n%s", got)
	}
}

func TestUsageCmdSessionScope(t *testing.T) {
	claudeDir := t.TempDir()
	writeUsageFixture(t, claudeDir, "-home-tester-repoA", "target-session", time.Hour,
		usageRecordLine("claude-sonnet-5", 50, 5, "2026-07-19T06:36:00.000Z"))
	writeUsageFixture(t, claudeDir, "-home-tester-repoB", "other-session", time.Hour,
		usageRecordLine("claude-opus-4-8", 999, 99, "2026-07-19T06:36:00.000Z"))

	var out, errw bytes.Buffer
	if code := Run([]string{"--session", "target-session", "--format", "json"}, claudeDir, &out, &errw); code != 0 {
		t.Fatalf("Run = %d, want 0; stderr:\n%s", code, errw.String())
	}

	var agg transcript.UsageAggregate
	if err := json.Unmarshal(out.Bytes(), &agg); err != nil {
		t.Fatalf("--format json output not valid JSON: %v\n%s", err, out.String())
	}
	if _, ok := agg.Models["claude-sonnet-5"]; !ok {
		t.Errorf("json output missing target session's model: %+v", agg.Models)
	}
	if _, ok := agg.Models["claude-opus-4-8"]; ok {
		t.Errorf("json output must not include the other session's model: %+v", agg.Models)
	}
}

func TestUsageCmdNoMatches(t *testing.T) {
	claudeDir := t.TempDir()
	var out, errw bytes.Buffer
	if code := Run([]string{"--days", "1"}, claudeDir, &out, &errw); code != 0 {
		t.Fatalf("Run on empty dir = %d, want 0", code)
	}
}

// findLineContaining returns the first line of s containing substr, or "".
func findLineContaining(s, substr string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if strings.Contains(line, substr) {
			return line
		}
	}
	return ""
}

// indexOfFields finds header somewhere in got allowing arbitrary whitespace between its fields
// (the real column widths are the implementation's choice), returning the byte index of the match
// start or -1.
func indexOfFields(got, header string) int {
	fields := strings.Fields(header)
	searchFrom := 0
	for {
		idx := strings.Index(got[searchFrom:], fields[0])
		if idx < 0 {
			return -1
		}
		start := searchFrom + idx
		pos := start
		ok := true
		for _, f := range fields {
			rest := got[pos:]
			fi := strings.Index(rest, f)
			if fi < 0 || fi > 40 { // field must appear soon after the previous one, same line/row
				ok = false
				break
			}
			pos += fi + len(f)
		}
		if ok {
			return start
		}
		searchFrom = start + 1
	}
}

// TestUsageShowBogusExitsTwo: an unrecognized --show value is a usage error, not a silent fallback.
func TestUsageShowBogusExitsTwo(t *testing.T) {
	claudeDir := t.TempDir()
	var out, errw bytes.Buffer
	code := Run([]string{"--show", "bogus"}, claudeDir, &out, &errw)
	if code != 2 {
		t.Fatalf("Run --show bogus = %d, want 2; stderr:\n%s", code, errw.String())
	}
	if !strings.Contains(errw.String(), "--show must be left, used, or total") {
		t.Errorf("stderr missing the --show error, got:\n%s", errw.String())
	}
}

// TestUsageShowModesRendering: each --show mode renders the quota column/detail its contract
// promises, from a fixed fake quota list via the fetchQuotas seam.
func TestUsageShowModesRendering(t *testing.T) {
	isolateAgentStores(t)
	stub := []quota{{Provider: "github", Account: "me", Plan: "Pro+", Window: "monthly", UsedPct: 25, Used: 1750, Limit: 7000, Resets: "2026-11-01"}}
	orig := fetchQuotas
	fetchQuotas = func() []quota { return stub }
	t.Cleanup(func() { fetchQuotas = orig })
	claudeDir := t.TempDir()

	cases := []struct {
		show string
		want []string
		not  []string
	}{
		{"left", []string{"LEFT", "75%", "5250 of 7000 credits left"}, []string{"USAGE"}},
		{"used", []string{"USAGE", "25%", "1750 / 7000 credits"}, []string{"LEFT"}},
		{"total", nil, []string{"PROVIDER QUOTAS"}},
	}
	for _, c := range cases {
		var out, errw bytes.Buffer
		if code := Run([]string{"--show", c.show}, claudeDir, &out, &errw); code != 0 {
			t.Fatalf("Run --show %s = %d, want 0; stderr:\n%s", c.show, code, errw.String())
		}
		got := out.String()
		for _, want := range c.want {
			if !strings.Contains(got, want) {
				t.Errorf("--show %s missing %q:\n%s", c.show, want, got)
			}
		}
		for _, notWant := range c.not {
			if strings.Contains(got, notWant) {
				t.Errorf("--show %s should not contain %q:\n%s", c.show, notWant, got)
			}
		}
		if !strings.Contains(got, "API KEYS") {
			t.Errorf("--show %s missing the API KEYS ledger section:\n%s", c.show, got)
		}
	}
}

// TestUsageShowTotalSkipsSubscriptionFetch: --show total never calls the subscription quota fetch
// (it only exists to show the API-key ledger section).
func TestUsageShowTotalSkipsSubscriptionFetch(t *testing.T) {
	called := false
	orig := fetchQuotas
	fetchQuotas = func() []quota { called = true; return nil }
	t.Cleanup(func() { fetchQuotas = orig })

	var out, errw bytes.Buffer
	if code := Run([]string{"--show", "total"}, t.TempDir(), &out, &errw); code != 0 {
		t.Fatalf("Run --show total = %d; stderr:\n%s", code, errw.String())
	}
	if called {
		t.Error("--show total must not fetch subscription quotas")
	}
	if strings.Contains(out.String(), "PROVIDER QUOTAS") {
		t.Errorf("--show total must not print the subscription quota block:\n%s", out.String())
	}
}

// TestUsageLedgerAggregationWindows: today/30d/all-time roll up correctly from a fixture ledger with
// timestamps fixed relative to the real now (the readLedger seam, not wall-clock fetch, is injected).
func TestUsageLedgerAggregationWindows(t *testing.T) {
	now := time.Now()
	entries := []tokenledger.Entry{
		{Ts: time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()), Provider: "openai", Model: "gpt-5", Input: 100, Output: 10}, // local midnight: today at any hour (now-1h broke 00:00–01:00)
		{Ts: now.Add(-20 * 24 * time.Hour), Provider: "openai", Model: "gpt-5", Input: 50, Output: 5},                                               // within 30d, not today
		{Ts: now.Add(-400 * 24 * time.Hour), Provider: "openai", Model: "gpt-5", Input: 9, Output: 1},                                               // all-time only
	}
	origRead, origQuotas := readLedger, fetchQuotas
	readLedger = func() ([]tokenledger.Entry, error) { return entries, nil }
	fetchQuotas = func() []quota { return nil }
	t.Cleanup(func() { readLedger, fetchQuotas = origRead, origQuotas })

	var out, errw bytes.Buffer
	if code := Run([]string{"--format", "json"}, t.TempDir(), &out, &errw); code != 0 {
		t.Fatalf("Run = %d; stderr:\n%s", code, errw.String())
	}
	var doc struct {
		API []ledgerAgg `json:"api"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out.String())
	}
	if len(doc.API) != 1 {
		t.Fatalf("api rows = %d, want 1: %+v", len(doc.API), doc.API)
	}
	a := doc.API[0]
	if a.Today != 110 {
		t.Errorf("Today = %d, want 110 (only the 1h-old entry)", a.Today)
	}
	if a.Last30d != 165 {
		t.Errorf("Last30d = %d, want 165 (1h + 20d entries)", a.Last30d)
	}
	if a.AllTime != 175 {
		t.Errorf("AllTime = %d, want 175 (every entry)", a.AllTime)
	}
}

// TestUsageLedgerEmptyPrintsStartedMessage: no ledger entries prints the "no per-token calls" line
// instead of a header with zero rows.
func TestUsageLedgerEmptyPrintsStartedMessage(t *testing.T) {
	origRead, origQuotas := readLedger, fetchQuotas
	readLedger = func() ([]tokenledger.Entry, error) { return nil, nil }
	fetchQuotas = func() []quota { return nil }
	t.Cleanup(func() { readLedger, fetchQuotas = origRead, origQuotas })

	var out, errw bytes.Buffer
	if code := Run(nil, t.TempDir(), &out, &errw); code != 0 {
		t.Fatalf("Run = %d; stderr:\n%s", code, errw.String())
	}
	if !strings.Contains(out.String(), "no per-token calls recorded yet") {
		t.Errorf("empty ledger missing the started message:\n%s", out.String())
	}
}

// aggregateLedger groups per profile: platform in Provider, profile in Profile; legacy lines read Provider as the profile.
func TestAggregateLedgerByProfile(t *testing.T) {
	now := time.Now()
	got := aggregateLedger([]tokenledger.Entry{
		{Ts: now, Provider: "databricks", Profile: "prod", Model: "m", Input: 10},
		{Ts: now, Provider: "databricks", Profile: "qa", Model: "m", Input: 5},
		{Ts: now, Provider: "openai", Model: "m", Input: 1},
	}, now)
	if len(got) != 3 || got[0].Profile != "prod" || got[1].Profile != "qa" || got[2].Profile != "openai" {
		t.Fatalf("rows = %+v", got)
	}
	if l := rowLabel(got[0].Provider, got[0].Profile); l != "databricks·prod" {
		t.Errorf("label = %q", l)
	}
	if l := rowLabel(got[2].Provider, got[2].Profile); l != "openai" {
		t.Errorf("label = %q", l)
	}
}

// TestScanUsageCountsOnlyTurnsInsideWindow: a session dir touched today still contributes only its fresh
// transcripts, and from those only the turns stamped inside the window; the daily buckets use the same set.
func TestScanUsageCountsOnlyTurnsInsideWindow(t *testing.T) {
	claudeDir := t.TempDir()
	fresh := writeUsageFixture(t, claudeDir, "-p", "s", time.Hour,
		usageRecordLine("claude-sonnet-5", 5000, 0, recentTS(10*24*time.Hour)), // old turn in a freshly appended file
		usageRecordLine("claude-sonnet-5", 100, 10, recentTS(time.Hour)))
	stale := filepath.Join(filepath.Dir(fresh), "agent-old.jsonl") // old file inside the same, freshly touched, session
	writeFile(t, stale, usageRecordLine("claude-opus-4-8", 7000, 0, recentTS(10*24*time.Hour)))
	old := time.Now().Add(-10 * 24 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	scan, err := scanUsage(claudeDir, "", 7, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.files) != 1 {
		t.Errorf("files = %v, want only the fresh transcript", scan.files)
	}
	if got := scan.agg.Models["claude-sonnet-5"]; got == nil || got.Total() != 110 {
		t.Errorf("sonnet = %+v, want 110 tokens (the 10-day-old turn is outside the window)", got)
	}
	if _, ok := scan.agg.Models["claude-opus-4-8"]; ok {
		t.Error("opus from the stale file leaked into the window")
	}
	if len(scan.daily) != 1 || scan.daily[0].Tokens != 110 || scan.daily[0].Turns != 1 {
		t.Errorf("daily = %+v, want one bucket of 110 tokens, 1 turn (same set as the aggregate)", scan.daily)
	}
}

// TestScanUsageServesSettledFilesFromCache: a second scan reads a settled transcript from the turn cache, with
// the same totals, so `usage` does not re-parse every file on every run.
func TestScanUsageServesSettledFilesFromCache(t *testing.T) {
	claudeDir := t.TempDir()
	p := writeUsageFixture(t, claudeDir, "-p", "s", time.Hour, usageRecordLine("claude-sonnet-5", 100, 10, recentTS(time.Hour)))
	first, err := scanUsage(claudeDir, "", 1, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(transcript.SummaryCachePath()); err != nil {
		t.Fatalf("scan wrote no turn cache: %v", err)
	}
	c := transcript.OpenSummaryCache(transcript.SummaryCachePath())
	if _, err := c.Responses(p); err != nil {
		t.Fatal(err)
	}
	if hits, misses := c.Stats(); hits != 1 || misses != 0 {
		t.Fatalf("cache after one scan: %d hits, %d misses; want 1, 0", hits, misses)
	}
	again, err := scanUsage(claudeDir, "", 1, io.Discard)
	if err != nil || !reflect.DeepEqual(again.agg, first.agg) {
		t.Fatalf("cached scan = %+v, %v; want %+v", again.agg, err, first.agg)
	}
}

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

// assertSectionOrder checks MODEL, PROVIDER QUOTAS, API KEYS and AGENTS all appear, in that order.
func assertSectionOrder(t *testing.T, s string) {
	t.Helper()
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

	assertSectionOrder(t, out.String())
	if !strings.Contains(errw.String(), "usage: github me at 100% (exceeded)") {
		t.Errorf("warn trailer missing from errw:\n%s", errw.String())
	}
}

// Disabled, the fetch never runs and yields nil; enabled, it runs and its rows come back with states set.
func TestStartQuotaFetch(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		called := false
		orig := fetchQuotas
		fetchQuotas = func() []quota { called = true; return []quota{{Provider: "github", UsedPct: 30}} }
		t.Cleanup(func() { fetchQuotas = orig })
		got := <-startQuotaFetch(enabled)
		if called != enabled {
			t.Errorf("startQuotaFetch(%v) called fetchQuotas = %v", enabled, called)
		}
		if !enabled && got != nil {
			t.Errorf("startQuotaFetch(false) result = %+v, want nil", got)
		}
		if enabled && (len(got) != 1 || got[0].LeftPct != 70 || got[0].State != "ok") {
			t.Errorf("startQuotaFetch(true) result = %+v, want LeftPct=70 State=ok", got)
		}
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

	assertUsageJSONKeys(t, doc, out.String())
	assertUsageJSONValues(t, doc)
}

// assertUsageJSONKeys: the pre-streaming key set is present and "api" is omitted for an empty ledger.
func assertUsageJSONKeys(t *testing.T, doc map[string]json.RawMessage, raw string) {
	t.Helper()
	wantKeys := []string{"models", "peak_input_tokens_per_min", "providers", "billing", "agents", "window_days"}
	for _, k := range wantKeys {
		if _, ok := doc[k]; !ok {
			t.Errorf("missing key %q in json doc:\n%s", k, raw)
		}
	}
	if _, ok := doc["api"]; ok {
		t.Errorf(`"api" key should be omitted (omitempty) when the ledger is empty:\n%s`, raw)
	}
}

// assertUsageJSONValues: the fixture's model tokens, the one github provider row and the 1-day window survive.
func assertUsageJSONValues(t *testing.T, doc map[string]json.RawMessage) {
	t.Helper()
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

	assertUsageJSONProviders(t, doc)
}

// assertUsageJSONProviders: one github provider row at 10% and a 1-day window.
func assertUsageJSONProviders(t *testing.T, doc map[string]json.RawMessage) {
	t.Helper()
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

// TestPrintQuotaWarnings: only warn/exceeded rows print to errw, in the documented format; ok rows are silent.
func TestPrintQuotaWarnings(t *testing.T) {
	qs := []quota{
		{Provider: "github", Account: "me", UsedPct: 50, State: "ok"},
		{Provider: "github", Account: "work", UsedPct: 85, State: "warn"},
		{Provider: "openai", Account: "", UsedPct: 100, State: "exceeded"},
	}
	var errw bytes.Buffer
	printQuotaWarnings(&errw, qs)
	got := errw.String()
	if strings.Contains(got, "me at 50%") {
		t.Errorf("an ok row must not warn:\n%s", got)
	}
	for _, want := range []string{
		"usage: github work at 85% (warn)\n",
		"usage: openai - at 100% (exceeded)\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}
