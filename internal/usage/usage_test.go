package usage

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/transcript"
	"github.com/edotau/claude-code/internal/usage/usageledger"
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

func usageRecordLine(model string, input, output int, ts string) string {
	return `{"type":"assistant","timestamp":"` + ts + `","message":{"model":"` + model +
		`","usage":{"input_tokens":` + strconv.Itoa(input) + `,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":` + strconv.Itoa(output) + `}}}`
}

// writeWorkflowAgentFixture writes one <session>/subagents/workflows/<runID>/agent-<name>.jsonl,
// its optional sibling .meta.json (workflowPhase), and backdates the files and directories.
func writeWorkflowAgentFixture(t *testing.T, claudeDir, project, session, runID, name, phase string, age time.Duration, lines ...string) string {
	t.Helper()
	dir := filepath.Join(claudeDir, "projects", project, session, "subagents", "workflows", runID)
	p := filepath.Join(dir, "agent-"+name+".jsonl")
	writeFile(t, p, strings.Join(lines, "\n"))
	mtime := time.Now().Add(-age)
	if phase != "" {
		metaPath := filepath.Join(dir, "agent-"+name+".meta.json")
		writeFile(t, metaPath, `{"workflowPhase":"`+phase+`"}`)
		if err := os.Chtimes(metaPath, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	sessionDir := filepath.Join(claudeDir, "projects", project, session)
	for _, path := range []string{p, dir, filepath.Dir(dir), filepath.Dir(filepath.Dir(dir)), sessionDir} {
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// writeWorkflowScript writes the sibling <session>/workflows/scripts/<name>-<runID>.js the workflow
// name is derived from.
func writeWorkflowScript(t *testing.T, claudeDir, project, session, name, runID string) {
	t.Helper()
	sessionDir := filepath.Join(claudeDir, "projects", project, session)
	info, err := os.Stat(sessionDir)
	if err != nil {
		t.Fatal(err)
	}
	mtime := info.ModTime()
	p := filepath.Join(sessionDir, "workflows", "scripts", name+"-"+runID+".js")
	writeFile(t, p, "// stub")
	for _, path := range []string{p, filepath.Dir(p), filepath.Dir(filepath.Dir(p)), sessionDir} {
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
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

// writeMainTranscript writes the session's own <session>.jsonl (sibling to the projects/<proj>/<session>
// dir) from real-byte pair content — the scriptPath run-name fallback scans this file.
func writeMainTranscript(t *testing.T, claudeDir, project, session, content string) {
	t.Helper()
	p := filepath.Join(claudeDir, "projects", project, session+".jsonl")
	writeFile(t, p, content)
}

func TestUsageCmdDaysScope(t *testing.T) {
	claudeDir := t.TempDir()
	writeUsageFixture(t, claudeDir, "-home-edotau-repoA", "session-recent", time.Hour,
		usageRecordLine("claude-sonnet-5", 100, 10, "2026-07-19T06:36:00.000Z"))
	writeUsageFixture(t, claudeDir, "-home-edotau-repoB", "session-old", 30*24*time.Hour,
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
	writeUsageFixture(t, claudeDir, "-home-edotau-repoA", "target-session", time.Hour,
		usageRecordLine("claude-sonnet-5", 50, 5, "2026-07-19T06:36:00.000Z"))
	writeUsageFixture(t, claudeDir, "-home-edotau-repoB", "other-session", time.Hour,
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

// TestUsageCmdIncludesWorkflowAgents: a workflow agent one level deeper than a plain subagent must
// still land in the default per-model table and count toward "files scanned".
func TestUsageCmdIncludesWorkflowAgents(t *testing.T) {
	claudeDir := t.TempDir()
	writeUsageFixture(t, claudeDir, "-home-edotau-repoA", "target-session", time.Hour,
		usageRecordLine("claude-sonnet-5", 50, 5, "2026-07-19T06:36:00.000Z"))
	writeWorkflowAgentFixture(t, claudeDir, "-home-edotau-repoA", "target-session", "wf_run1", "a", "Find", time.Hour,
		usageRecordLine("claude-opus-4-8", 700, 70, "2026-07-19T06:37:00.000Z"))

	var out, errw bytes.Buffer
	if code := Run([]string{"--session", "target-session", "--format", "json"}, claudeDir, &out, &errw); code != 0 {
		t.Fatalf("Run = %d, want 0; stderr:\n%s", code, errw.String())
	}
	var agg transcript.UsageAggregate
	if err := json.Unmarshal(out.Bytes(), &agg); err != nil {
		t.Fatalf("--format json output not valid JSON: %v\n%s", err, out.String())
	}
	if _, ok := agg.Models["claude-sonnet-5"]; !ok {
		t.Errorf("json output missing plain subagent's model: %+v", agg.Models)
	}
	opus, ok := agg.Models["claude-opus-4-8"]
	if !ok {
		t.Fatalf("json output missing workflow agent's model: %+v", agg.Models)
	}
	if opus.InputTokens != 700 {
		t.Errorf("workflow agent input_tokens = %d, want 700 (double-count check)", opus.InputTokens)
	}
}

// TestUsageCmdIncludesWorkflowAgentsWithinDays: the --days path (usageFilesWithinDays) must also
// walk the deeper subagents/workflows/<runId>/agent-*.jsonl glob, not just --session.
func TestUsageCmdIncludesWorkflowAgentsWithinDays(t *testing.T) {
	claudeDir := t.TempDir()
	writeWorkflowAgentFixture(t, claudeDir, "-home-edotau-repoA", "session-1", "wf_run1", "a", "Find", time.Hour,
		usageRecordLine("claude-opus-4-8", 400, 40, "2026-07-19T06:37:00.000Z"))

	var out, errw bytes.Buffer
	if code := Run([]string{"--days", "1"}, claudeDir, &out, &errw); code != 0 {
		t.Fatalf("Run = %d, want 0; stderr:\n%s", code, errw.String())
	}
	if !strings.Contains(out.String(), "claude-opus-4-8") {
		t.Errorf("--days table missing workflow agent's model:\n%s", out.String())
	}
}

// TestUsageCmdWorkflowsFlag: --workflows prints a header (run id + derived name), one row per
// phase (including an unphased agent under "(none)"), and a TOTAL row.
func TestUsageCmdWorkflowsFlag(t *testing.T) {
	claudeDir := t.TempDir()
	writeWorkflowAgentFixture(t, claudeDir, "-home-edotau-repoA", "target-session", "wf_2ac0b502-bf2", "a", "Find", time.Hour,
		usageRecordLine("claude-sonnet-5", 100, 10, "2026-07-19T06:36:00.000Z"),
		usageRecordLine("claude-sonnet-5", 100, 10, "2026-07-19T06:36:30.000Z"))
	writeWorkflowAgentFixture(t, claudeDir, "-home-edotau-repoA", "target-session", "wf_2ac0b502-bf2", "b", "", time.Hour,
		usageRecordLine("claude-opus-4-8", 200, 20, "2026-07-19T06:38:00.000Z"))
	writeWorkflowScript(t, claudeDir, "-home-edotau-repoA", "target-session", "harness-cut-audit-wf", "wf_2ac0b502-bf2")

	var out, errw bytes.Buffer
	code := Run([]string{"--workflows", "--session", "target-session"}, claudeDir, &out, &errw)
	if code != 0 {
		t.Fatalf("Run --workflows = %d, want 0; stderr:\n%s", code, errw.String())
	}
	got := out.String()
	for _, want := range []string{"wf_2ac0b502-bf2", "harness-cut-audit-wf", "Find", "(none)", "TOTAL"} {
		if !strings.Contains(got, want) {
			t.Errorf("--workflows output missing %q:\n%s", want, got)
		}
	}
}

// TestUsageCmdWorkflowsNewestFirst: runs are ordered by their last transcript timestamp, not by the
// (random) run id string.
func TestUsageCmdWorkflowsNewestFirst(t *testing.T) {
	claudeDir := t.TempDir()
	writeWorkflowAgentFixture(t, claudeDir, "-home-edotau-repoA", "target-session", "wf_aaa-old", "a", "Find", time.Hour,
		usageRecordLine("claude-sonnet-5", 10, 1, "2026-07-19T06:00:00.000Z"))
	writeWorkflowAgentFixture(t, claudeDir, "-home-edotau-repoA", "target-session", "wf_zzz-new", "a", "Find", time.Hour,
		usageRecordLine("claude-sonnet-5", 10, 1, "2026-07-19T09:00:00.000Z"))

	var out, errw bytes.Buffer
	if code := Run([]string{"--workflows", "--session", "target-session"}, claudeDir, &out, &errw); code != 0 {
		t.Fatalf("Run --workflows = %d, want 0; stderr:\n%s", code, errw.String())
	}
	got := out.String()
	newIdx := strings.Index(got, "wf_zzz-new")
	oldIdx := strings.Index(got, "wf_aaa-old")
	if newIdx < 0 || oldIdx < 0 || newIdx > oldIdx {
		t.Errorf("newest run must print first, got order in:\n%s", got)
	}
}

// TestUsageCmdWorkflowsRejectsJSON: --workflows has no JSON form, so --workflows --format json is a usage
// error rather than silently falling back to the per-model aggregate JSON.
func TestUsageCmdWorkflowsRejectsJSON(t *testing.T) {
	claudeDir := t.TempDir()
	var out, errw bytes.Buffer
	if code := Run([]string{"--workflows", "--format", "json"}, claudeDir, &out, &errw); code == 0 {
		t.Errorf("Run --workflows --format json = %d, want nonzero", code)
	}
}

// TestWorkflowPhaseFromMetaMalformed: a truncated/malformed meta.json must fall back to "" (routed
// to "(none)" by the caller) and warn on errw, mirroring loadWorkflowRun's read-error pattern.
func TestWorkflowPhaseFromMetaMalformed(t *testing.T) {
	dir := t.TempDir()
	transcriptPath := filepath.Join(dir, "agent-a.jsonl")
	writeFile(t, transcriptPath, "")
	writeFile(t, filepath.Join(dir, "agent-a.meta.json"), `{"workflowPhase":`) // truncated JSON

	var errw bytes.Buffer
	phase := workflowPhaseFromMeta(transcriptPath, &errw)
	if phase != "" {
		t.Errorf("workflowPhaseFromMeta on malformed meta = %q, want \"\"", phase)
	}
	if !strings.Contains(errw.String(), "usage: warning: skipping meta for") {
		t.Errorf("workflowPhaseFromMeta on malformed meta printed no warning, got:\n%s", errw.String())
	}
}

// TestUsageCmdWorkflowsModelColumnSingleModel: a one-model phase (real haiku-only bytes) prints the
// short name (date suffix stripped), not the raw model id, and the TOTAL row carries it too.
func TestUsageCmdWorkflowsModelColumnSingleModel(t *testing.T) {
	claudeDir := t.TempDir()
	writeWorkflowAgentFixture(t, claudeDir, "-home-edotau-repoA", "target-session", "wf_65ae8cee-79a", "a", "Detect", time.Hour,
		readTestdata(t, "wf_65ae8cee-79a-agent-a.jsonl"))

	var out, errw bytes.Buffer
	if code := Run([]string{"--workflows", "--session", "target-session"}, claudeDir, &out, &errw); code != 0 {
		t.Fatalf("Run --workflows = %d, want 0; stderr:\n%s", code, errw.String())
	}
	got := out.String()
	if !strings.Contains(got, "haiku-4-5") {
		t.Errorf("MODEL column missing short name haiku-4-5:\n%s", got)
	}
	if strings.Contains(got, "claude-haiku-4-5-20251001") {
		t.Errorf("MODEL column must print the short name, not the raw model id:\n%s", got)
	}
	const header = "MODEL AGENTS TURNS INPUT CACHE-READ CACHE-CREATE OUTPUT"
	headerIdx := indexOfFields(got, header)
	if headerIdx < 0 {
		t.Fatalf("missing rollup header %q in:\n%s", header, got)
	}
	if n := strings.Count(got[headerIdx+len(header):], "haiku-4-5"); n != 1 {
		t.Errorf("single-model run must print exactly one rollup row, found %d after the header:\n%s", n, got)
	}
}

// TestUsageCmdWorkflowsModelColumnMixed: a phase folding two real agents on different models (haiku
// + sonnet, same "▸ gate-loop" phase from the live wf_529c9ef1-350 run) joins them with '+'.
func TestUsageCmdWorkflowsModelColumnMixed(t *testing.T) {
	claudeDir := t.TempDir()
	writeWorkflowAgentFixture(t, claudeDir, "-home-edotau-repoA", "target-session", "wf_529c9ef1-350", "haiku", "gate-loop", time.Hour,
		readTestdata(t, "wf_529c9ef1-350-agent-a1f2df362eb625d28.jsonl"))
	writeWorkflowAgentFixture(t, claudeDir, "-home-edotau-repoA", "target-session", "wf_529c9ef1-350", "sonnet", "gate-loop", time.Hour,
		readTestdata(t, "wf_529c9ef1-350-agent-a270aa99ac8d4f124.jsonl"))

	var out, errw bytes.Buffer
	if code := Run([]string{"--workflows", "--session", "target-session"}, claudeDir, &out, &errw); code != 0 {
		t.Fatalf("Run --workflows = %d, want 0; stderr:\n%s", code, errw.String())
	}
	got := out.String()
	phaseLine := findLineContaining(got, "gate-loop")
	if phaseLine == "" {
		t.Fatalf("no phase row for gate-loop:\n%s", got)
	}
	if !strings.Contains(phaseLine, "haiku-4-5") || !strings.Contains(phaseLine, "sonnet-5") {
		t.Errorf("mixed phase row must list both short names, got: %q", phaseLine)
	}
	if !strings.Contains(phaseLine, "haiku-4-5+sonnet-5") && !strings.Contains(phaseLine, "sonnet-5+haiku-4-5") {
		t.Errorf("mixed phase row must '+'-join the short names, got: %q", phaseLine)
	}
}

// TestUsageCmdWorkflowsPerRunRollup: after the TOTAL row, one rollup block per run with the fixed
// header, one row per model, sorted by cache-read desc (sonnet's real cache-read total here beats
// haiku's).
func TestUsageCmdWorkflowsPerRunRollup(t *testing.T) {
	claudeDir := t.TempDir()
	writeWorkflowAgentFixture(t, claudeDir, "-home-edotau-repoA", "target-session", "wf_529c9ef1-350", "haiku", "gate-loop", time.Hour,
		readTestdata(t, "wf_529c9ef1-350-agent-a1f2df362eb625d28.jsonl"))
	writeWorkflowAgentFixture(t, claudeDir, "-home-edotau-repoA", "target-session", "wf_529c9ef1-350", "sonnet", "gate-loop", time.Hour,
		readTestdata(t, "wf_529c9ef1-350-agent-a270aa99ac8d4f124.jsonl"))

	var out, errw bytes.Buffer
	if code := Run([]string{"--workflows", "--session", "target-session"}, claudeDir, &out, &errw); code != 0 {
		t.Fatalf("Run --workflows = %d, want 0; stderr:\n%s", code, errw.String())
	}
	got := out.String()
	const header = "MODEL AGENTS TURNS INPUT CACHE-READ CACHE-CREATE OUTPUT"
	headerIdx := indexOfFields(got, header)
	if headerIdx < 0 {
		t.Fatalf("missing rollup header %q in:\n%s", header, got)
	}
	rollupSonnetIdx := strings.Index(got[headerIdx:], "sonnet-5")
	rollupHaikuIdx := strings.Index(got[headerIdx:], "haiku-4-5")
	if rollupSonnetIdx < 0 || rollupHaikuIdx < 0 {
		t.Fatalf("rollup block missing a model row, got:\n%s", got[headerIdx:])
	}
	if rollupSonnetIdx > rollupHaikuIdx {
		t.Errorf("rollup must sort by cache-read desc (sonnet's cache-read total is larger): sonnet@%d haiku@%d in:\n%s",
			rollupSonnetIdx, rollupHaikuIdx, got[headerIdx:])
	}
}

// TestUsageCmdWorkflowsAllRunsByModel: after every per-run block, one "ALL RUNS BY MODEL" block sums
// token/agent/turn counts across the listed runs, printed exactly once.
func TestUsageCmdWorkflowsAllRunsByModel(t *testing.T) {
	claudeDir := t.TempDir()
	writeWorkflowAgentFixture(t, claudeDir, "-home-edotau-repoA", "target-session", "wf_65ae8cee-79a", "a", "Detect", time.Hour,
		readTestdata(t, "wf_65ae8cee-79a-agent-a.jsonl"))
	writeWorkflowAgentFixture(t, claudeDir, "-home-edotau-repoA", "target-session", "wf_529c9ef1-350", "sonnet", "gate-loop", 2*time.Hour,
		readTestdata(t, "wf_529c9ef1-350-agent-a270aa99ac8d4f124.jsonl"))

	var out, errw bytes.Buffer
	if code := Run([]string{"--workflows", "--session", "target-session"}, claudeDir, &out, &errw); code != 0 {
		t.Fatalf("Run --workflows = %d, want 0; stderr:\n%s", code, errw.String())
	}
	got := out.String()
	if n := strings.Count(got, "ALL RUNS BY MODEL"); n != 1 {
		t.Fatalf("want exactly one ALL RUNS BY MODEL block, got %d in:\n%s", n, got)
	}
	allIdx := strings.Index(got, "ALL RUNS BY MODEL")
	rest := got[allIdx:]
	if !strings.Contains(rest, "haiku-4-5") || !strings.Contains(rest, "sonnet-5") {
		t.Errorf("ALL RUNS BY MODEL block must list every model across both runs:\n%s", rest)
	}
	if strings.Count(rest, "wf_65ae8cee-79a") > 0 || strings.Count(rest, "wf_529c9ef1-350") > 0 {
		t.Errorf("ALL RUNS BY MODEL block must be keyed by model, not repeat run ids:\n%s", rest)
	}
}

// TestUsageCmdWorkflowsNoRunsNoAllRunsBlock: with no matching runs, no ALL RUNS BY MODEL block prints.
func TestUsageCmdWorkflowsNoRunsNoAllRunsBlock(t *testing.T) {
	claudeDir := t.TempDir()
	var out, errw bytes.Buffer
	if code := Run([]string{"--workflows", "--days", "1"}, claudeDir, &out, &errw); code != 0 {
		t.Fatalf("Run --workflows on empty dir = %d, want 0", code)
	}
	if strings.Contains(out.String(), "ALL RUNS BY MODEL") {
		t.Errorf("empty run set must not print an ALL RUNS BY MODEL block:\n%s", out.String())
	}
}

// TestWorkflowNameForRunScriptPathFallback: a {scriptPath} run has no scripts/*-<runId>.js (that glob
// only matches inline/by-name runs), so the name comes from the main transcript's tool_use/tool_result
// pair (real bytes from this session's own launch of implement-and-verify.js as wf_c877ead7-adf).
func TestWorkflowNameForRunScriptPathFallback(t *testing.T) {
	claudeDir := t.TempDir()
	project, session, runID := "-home-edotau-repoA", "target-session", "wf_c877ead7-adf"
	writeMainTranscript(t, claudeDir, project, session, readTestdata(t, "session-scriptpath-pair.jsonl"))
	runDir := filepath.Join(claudeDir, "projects", project, session, "subagents", "workflows", runID)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}

	name := workflowNameForRun(runDir, runID)
	if name != "implement-and-verify" {
		t.Errorf("workflowNameForRun scriptPath fallback = %q, want %q", name, "implement-and-verify")
	}
}

func TestScanWorkflowNamesPairsResults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	writeFile(t, path, strings.Join([]string{
		`not json`,
		`{"type":"assistant","message":{"content":"not blocks"}}`,
		`{"type":"user","message":{"content":"not blocks"}}`,
		`{"type":"api-request-blob","message":{"content":[{"type":"tool_use","id":"ignored","input":{"name":"ignored"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"","input":{"name":"no-id"}},{"type":"tool_use","id":"a","input":{"name":"named","scriptPath":"/tmp/unused.js"}},{"type":"tool_use","id":"b","input":{"scriptPath":"/tmp/from-path.js"}},{"type":"tool_use","id":"inline","input":{}}]}}`,
		`{"type":"user","message":{"content":[{"type":"text","content":"Run ID: wf_text"},{"type":"tool_result","tool_use_id":"ignored","content":"Run ID: wf_ignored"},{"type":"tool_result","tool_use_id":"missing","content":"Run ID: wf_unknown"},{"type":"tool_result","tool_use_id":"inline","content":"Run ID: wf_inline"},{"type":"tool_result","tool_use_id":"a","content":"no run id"},{"type":"tool_result","tool_use_id":"a","content":"Run ID: wf_named"},{"type":"tool_result","tool_use_id":"b","content":"Run ID: wf_named"},{"type":"tool_result","tool_use_id":"b","content":[{"type":"text","text":"Transcript dir: /tmp/wf_path"}]}]}}`,
	}, "\n"))
	names := scanWorkflowNamesFromTranscript(path)
	if len(names) != 2 || names["wf_named"] != "named" || names["wf_path"] != "from-path" {
		t.Fatalf("workflow names = %v", names)
	}
}

// With no matching script or transcript launch, the run id itself is the name.
func TestWorkflowNameForRunNoFallbackMatch(t *testing.T) {
	claudeDir := t.TempDir()
	project, session, runID := "-home-edotau-repoA", "target-session", "wf_no_such_run"
	runDir := filepath.Join(claudeDir, "projects", project, session, "subagents", "workflows", runID)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}

	name := workflowNameForRun(runDir, runID)
	if name != runID {
		t.Errorf("workflowNameForRun with no match = %q, want runID %q", name, runID)
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

// WorkflowRunDirs/WorkflowAgentFiles/WorkflowPhaseFromMeta are exported solely for internal/agent;
// this pins their contract against the same fixture shapes workflowRunDirsForScope already exercises.
func TestExportedWorkflowWrappersForAgentTree(t *testing.T) {
	dir := t.TempDir()
	sessionDir := filepath.Join(dir, "projects", "p", "sess0001")
	writeWorkflowAgentFixture(t, dir, "p", "sess0001", "wf_abc", "one", "Detect", time.Minute,
		usageRecordLine("claude-sonnet-4-5", 10, 5, "2026-10-18T00:00:00Z"))
	writeWorkflowAgentFixture(t, dir, "p", "sess0001", "wf_abc", "two", "", time.Minute,
		usageRecordLine("claude-sonnet-4-5", 20, 8, "2026-10-18T00:01:00Z"))

	runDirs := WorkflowRunDirs(sessionDir)
	if len(runDirs) != 1 || filepath.Base(runDirs[0]) != "wf_abc" {
		t.Fatalf("WorkflowRunDirs = %v, want one dir named wf_abc", runDirs)
	}

	files := WorkflowAgentFiles(runDirs[0])
	if len(files) != 2 {
		t.Fatalf("WorkflowAgentFiles = %v, want 2 agent transcripts", files)
	}
	phases := map[string]bool{}
	for _, f := range files {
		phases[WorkflowPhaseFromMeta(f)] = true
	}
	if !phases["Detect"] {
		t.Error("WorkflowPhaseFromMeta should read \"Detect\" from agent-one's sidecar meta")
	}
	if !phases[""] {
		t.Error("WorkflowPhaseFromMeta should return \"\" for agent-two (no sidecar meta written)")
	}
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
	entries := []usageledger.Entry{
		{Ts: time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()), Provider: "openai", Model: "gpt-5", Input: 100, Output: 10}, // local midnight: today at any hour (now-1h broke 00:00–01:00)
		{Ts: now.Add(-20 * 24 * time.Hour), Provider: "openai", Model: "gpt-5", Input: 50, Output: 5},                                               // within 30d, not today
		{Ts: now.Add(-400 * 24 * time.Hour), Provider: "openai", Model: "gpt-5", Input: 9, Output: 1},                                               // all-time only
	}
	origRead, origQuotas := readLedger, fetchQuotas
	readLedger = func() ([]usageledger.Entry, error) { return entries, nil }
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
	readLedger = func() ([]usageledger.Entry, error) { return nil, nil }
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
	got := aggregateLedger([]usageledger.Entry{
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
