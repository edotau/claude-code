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
// backdates the session dir's mtime — usageFilesWithinDays filters on it.
func writeUsageFixture(t *testing.T, claudeDir, project, session string, age time.Duration, lines ...string) string {
	t.Helper()
	p := filepath.Join(claudeDir, "projects", project, session, "subagents", "agent-a.jsonl")
	writeFile(t, p, strings.Join(lines, "\n"))
	mtime := time.Now().Add(-age)
	sessionDir := filepath.Join(claudeDir, "projects", project, session)
	if err := os.Chtimes(sessionDir, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return p
}

func usageRecordLine(model string, input, output int, ts string) string {
	return `{"type":"assistant","timestamp":"` + ts + `","message":{"model":"` + model +
		`","usage":{"input_tokens":` + strconv.Itoa(input) + `,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":` + strconv.Itoa(output) + `}}}`
}

// writeWorkflowAgentFixture writes one <session>/subagents/workflows/<runID>/agent-<name>.jsonl,
// its optional sibling .meta.json (workflowPhase), and backdates the session dir's mtime.
func writeWorkflowAgentFixture(t *testing.T, claudeDir, project, session, runID, name, phase string, age time.Duration, lines ...string) string {
	t.Helper()
	dir := filepath.Join(claudeDir, "projects", project, session, "subagents", "workflows", runID)
	p := filepath.Join(dir, "agent-"+name+".jsonl")
	writeFile(t, p, strings.Join(lines, "\n"))
	if phase != "" {
		writeFile(t, filepath.Join(dir, "agent-"+name+".meta.json"), `{"workflowPhase":"`+phase+`"}`)
	}
	sessionDir := filepath.Join(claudeDir, "projects", project, session)
	mtime := time.Now().Add(-age)
	if err := os.Chtimes(sessionDir, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeWorkflowScript writes the sibling <session>/workflows/scripts/<name>-<runID>.js the workflow
// name is derived from.
func writeWorkflowScript(t *testing.T, claudeDir, project, session, name, runID string) {
	t.Helper()
	p := filepath.Join(claudeDir, "projects", project, session, "workflows", "scripts", name+"-"+runID+".js")
	writeFile(t, p, "// stub")
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
	if code := Run([]string{"--session", "target-session", "--json"}, claudeDir, &out, &errw); code != 0 {
		t.Fatalf("Run = %d, want 0; stderr:\n%s", code, errw.String())
	}

	var agg transcript.UsageAggregate
	if err := json.Unmarshal(out.Bytes(), &agg); err != nil {
		t.Fatalf("--json output not valid JSON: %v\n%s", err, out.String())
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
	if code := Run([]string{"--session", "target-session", "--json"}, claudeDir, &out, &errw); code != 0 {
		t.Fatalf("Run = %d, want 0; stderr:\n%s", code, errw.String())
	}
	var agg transcript.UsageAggregate
	if err := json.Unmarshal(out.Bytes(), &agg); err != nil {
		t.Fatalf("--json output not valid JSON: %v\n%s", err, out.String())
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

// TestUsageCmdWorkflowsRejectsJSON: --workflows has no JSON form, so --workflows --json is a usage
// error rather than silently falling back to the per-model aggregate JSON.
func TestUsageCmdWorkflowsRejectsJSON(t *testing.T) {
	claudeDir := t.TempDir()
	var out, errw bytes.Buffer
	if code := Run([]string{"--workflows", "--json"}, claudeDir, &out, &errw); code == 0 {
		t.Errorf("Run --workflows --json = %d, want nonzero", code)
	}
}

// TestUsageCmdWorkflowsNoMatches: an empty tree exits 0 under --workflows, matching the plain path.
func TestUsageCmdWorkflowsNoMatches(t *testing.T) {
	claudeDir := t.TempDir()
	var out, errw bytes.Buffer
	if code := Run([]string{"--workflows", "--days", "1"}, claudeDir, &out, &errw); code != 0 {
		t.Fatalf("Run --workflows on empty dir = %d, want 0", code)
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
