package usage

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/transcript"
)

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

// writeMainTranscript writes the session's own <session>.jsonl (sibling to the projects/<proj>/<session>
// dir) from real-byte pair content — the scriptPath run-name fallback scans this file.
func writeMainTranscript(t *testing.T, claudeDir, project, session, content string) {
	t.Helper()
	p := filepath.Join(claudeDir, "projects", project, session+".jsonl")
	writeFile(t, p, content)
}

// TestUsageCmdIncludesWorkflowAgents: a workflow agent one level deeper than a plain subagent must
// still land in the default per-model table and count toward "files scanned".
func TestUsageCmdIncludesWorkflowAgents(t *testing.T) {
	claudeDir := t.TempDir()
	writeUsageFixture(t, claudeDir, "-home-tester-repoA", "target-session", time.Hour,
		usageRecordLine("claude-sonnet-5", 50, 5, "2026-07-19T06:36:00.000Z"))
	writeWorkflowAgentFixture(t, claudeDir, "-home-tester-repoA", "target-session", "wf_run1", "a", "Find", time.Hour,
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
	writeWorkflowAgentFixture(t, claudeDir, "-home-tester-repoA", "session-1", "wf_run1", "a", "Find", time.Hour,
		usageRecordLine("claude-opus-4-8", 400, 40, recentTS(time.Hour)))

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
	writeWorkflowAgentFixture(t, claudeDir, "-home-tester-repoA", "target-session", "wf_2ac0b502-bf2", "a", "Find", time.Hour,
		usageRecordLine("claude-sonnet-5", 100, 10, "2026-07-19T06:36:00.000Z"),
		usageRecordLine("claude-sonnet-5", 100, 10, "2026-07-19T06:36:30.000Z"))
	writeWorkflowAgentFixture(t, claudeDir, "-home-tester-repoA", "target-session", "wf_2ac0b502-bf2", "b", "", time.Hour,
		usageRecordLine("claude-opus-4-8", 200, 20, "2026-07-19T06:38:00.000Z"))
	writeWorkflowScript(t, claudeDir, "-home-tester-repoA", "target-session", "harness-cut-audit-wf", "wf_2ac0b502-bf2")

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
	writeWorkflowAgentFixture(t, claudeDir, "-home-tester-repoA", "target-session", "wf_aaa-old", "a", "Find", time.Hour,
		usageRecordLine("claude-sonnet-5", 10, 1, "2026-07-19T06:00:00.000Z"))
	writeWorkflowAgentFixture(t, claudeDir, "-home-tester-repoA", "target-session", "wf_zzz-new", "a", "Find", time.Hour,
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

// TestUsageCmdWorkflowsModelColumnSingleModel: a one-model phase (real haiku-only bytes) prints the
// short name (date suffix stripped), not the raw model id, and the TOTAL row carries it too.
func TestUsageCmdWorkflowsModelColumnSingleModel(t *testing.T) {
	claudeDir := t.TempDir()
	writeWorkflowAgentFixture(t, claudeDir, "-home-tester-repoA", "target-session", "wf_65ae8cee-79a", "a", "Detect", time.Hour,
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
	writeWorkflowAgentFixture(t, claudeDir, "-home-tester-repoA", "target-session", "wf_529c9ef1-350", "haiku", "gate-loop", time.Hour,
		readTestdata(t, "wf_529c9ef1-350-agent-a1f2df362eb625d28.jsonl"))
	writeWorkflowAgentFixture(t, claudeDir, "-home-tester-repoA", "target-session", "wf_529c9ef1-350", "sonnet", "gate-loop", time.Hour,
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
	writeWorkflowAgentFixture(t, claudeDir, "-home-tester-repoA", "target-session", "wf_529c9ef1-350", "haiku", "gate-loop", time.Hour,
		readTestdata(t, "wf_529c9ef1-350-agent-a1f2df362eb625d28.jsonl"))
	writeWorkflowAgentFixture(t, claudeDir, "-home-tester-repoA", "target-session", "wf_529c9ef1-350", "sonnet", "gate-loop", time.Hour,
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
	writeWorkflowAgentFixture(t, claudeDir, "-home-tester-repoA", "target-session", "wf_65ae8cee-79a", "a", "Detect", time.Hour,
		readTestdata(t, "wf_65ae8cee-79a-agent-a.jsonl"))
	writeWorkflowAgentFixture(t, claudeDir, "-home-tester-repoA", "target-session", "wf_529c9ef1-350", "sonnet", "gate-loop", 2*time.Hour,
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
	project, session, runID := "-home-tester-repoA", "target-session", "wf_c877ead7-adf"
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
	project, session, runID := "-home-tester-repoA", "target-session", "wf_no_such_run"
	runDir := filepath.Join(claudeDir, "projects", project, session, "subagents", "workflows", runID)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}

	name := workflowNameForRun(runDir, runID)
	if name != runID {
		t.Errorf("workflowNameForRun with no match = %q, want runID %q", name, runID)
	}
}
