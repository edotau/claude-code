package hooks

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// subagentStopFields fills the real-shape testdata/subagentstop.json template.
type subagentStopFields struct {
	sessionID           string
	transcriptPath      string
	agentID             string
	agentType           string
	stopHookActive      bool
	agentTranscriptPath string
}

// buildSubagentStopPayload substitutes placeholders in the captured real payload, keeping every key.
func buildSubagentStopPayload(t *testing.T, f subagentStopFields) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "subagentstop.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for token, val := range map[string]string{
		"{{SESSION_ID}}":            f.sessionID,
		"{{TRANSCRIPT_PATH}}":       f.transcriptPath,
		"{{AGENT_ID}}":              f.agentID,
		"{{AGENT_TYPE}}":            f.agentType,
		"{{STOP_HOOK_ACTIVE}}":      strconv.FormatBool(f.stopHookActive),
		"{{AGENT_TRANSCRIPT_PATH}}": f.agentTranscriptPath,
	} {
		s = strings.ReplaceAll(s, token, val)
	}
	return s
}

// editToolUseLine mirrors the real subagent transcript shape captured from a live Claude Code run:
// a top-level "type":"assistant" record whose message.content carries the Edit/Write tool_use.
func editToolUseLine(t *testing.T, agentID, toolName, filePath string) string {
	t.Helper()
	rec := map[string]any{
		"type":    "assistant",
		"agentId": agentID,
		"message": map[string]any{
			"model": "claude-sonnet-5",
			"id":    "msg_test",
			"type":  "message",
			"role":  "assistant",
			"content": []map[string]any{{
				"type": "tool_use",
				"id":   "toolu_test",
				"name": toolName,
				"input": map[string]any{
					"file_path": filePath,
				},
			}},
		},
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// writeTranscript writes one or more JSONL lines to a fresh subagent transcript file, returning its path.
func writeTranscript(t *testing.T, dir string, lines ...string) string {
	t.Helper()
	path := filepath.Join(dir, "agent-t1.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSimplifyAfterEditsStopHookActiveProceeds(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := t.TempDir()
	tr := writeTranscript(t, dir, editToolUseLine(t, "a1", "Edit", "/r/a.go"))
	payload := buildSubagentStopPayload(t, subagentStopFields{
		agentID: "a1", agentType: "general-purpose", stopHookActive: true, agentTranscriptPath: tr,
	})
	var stderr bytes.Buffer
	if code := SimplifyAfterEdits(strings.NewReader(payload), &stderr); code != ExitProceed {
		t.Errorf("stop_hook_active must proceed, got %d: %s", code, stderr.String())
	}
}

func TestSimplifyAfterEditsEmptyAgentTranscriptPathProceeds(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	payload := buildSubagentStopPayload(t, subagentStopFields{
		agentID: "a1", agentType: "general-purpose", agentTranscriptPath: "",
	})
	var stderr bytes.Buffer
	if code := SimplifyAfterEdits(strings.NewReader(payload), &stderr); code != ExitProceed {
		t.Errorf("empty agent_transcript_path must proceed, got %d: %s", code, stderr.String())
	}
}

func TestSimplifyAfterEditsSkipsSkipAgents(t *testing.T) {
	for agentType := range skipSimplifyAgents {
		t.Run(agentType, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			dir := t.TempDir()
			tr := writeTranscript(t, dir, editToolUseLine(t, "a1", "Edit", "/r/a.go"))
			payload := buildSubagentStopPayload(t, subagentStopFields{
				agentID: "a1", agentType: agentType, agentTranscriptPath: tr,
			})
			var stderr bytes.Buffer
			if code := SimplifyAfterEdits(strings.NewReader(payload), &stderr); code != ExitProceed {
				t.Errorf("skip-listed agent_type %q must proceed, got %d: %s", agentType, code, stderr.String())
			}
		})
	}
}

func TestSimplifyAfterEditsBlocksOnSourceEditThenProceedsOnce(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := t.TempDir()
	tr := writeTranscript(t, dir, editToolUseLine(t, "a1", "Write", "/r/a.go"))
	payload := buildSubagentStopPayload(t, subagentStopFields{
		agentID: "a1", agentType: "general-purpose", agentTranscriptPath: tr,
	})
	var stderr bytes.Buffer
	if code := SimplifyAfterEdits(strings.NewReader(payload), &stderr); code != ExitBlock {
		t.Fatalf("first stop after a source edit must block, got %d: %s", code, stderr.String())
	}
	out := stderr.String()
	for _, want := range []string{"skills/simplicity/SKILL.md", "claude-code review quality", "/r/a.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("block message must name %q: %s", want, out)
		}
	}
	// Second stop for the same subagent: the flag written by the first block makes it proceed.
	var stderr2 bytes.Buffer
	if code := SimplifyAfterEdits(strings.NewReader(payload), &stderr2); code != ExitProceed {
		t.Errorf("second stop for the same subagent must proceed (block-once), got %d: %s", code, stderr2.String())
	}
}

func TestSimplifyAfterEditsNonSourceEditsProceed(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := t.TempDir()
	tr := writeTranscript(t, dir,
		editToolUseLine(t, "a1", "Write", "/r/notes.txt"),
		editToolUseLine(t, "a1", "Edit", "/r/README.md"),
	)
	payload := buildSubagentStopPayload(t, subagentStopFields{
		agentID: "a1", agentType: "general-purpose", agentTranscriptPath: tr,
	})
	var stderr bytes.Buffer
	if code := SimplifyAfterEdits(strings.NewReader(payload), &stderr); code != ExitProceed {
		t.Errorf(".txt/.md-only edits must proceed, got %d: %s", code, stderr.String())
	}
}

func TestSimplifyAfterEditsMetaJSONFallbackForAgentType(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := t.TempDir()
	tr := writeTranscript(t, dir, editToolUseLine(t, "a1", "Edit", "/r/a.go"))
	meta := strings.TrimSuffix(tr, ".jsonl") + ".meta.json"
	if err := os.WriteFile(meta, []byte(`{"agentType":"Explore"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// agent_type absent from the payload itself: the hook must fall back to the meta.json sidecar.
	payload := buildSubagentStopPayload(t, subagentStopFields{
		agentID: "a1", agentType: "", agentTranscriptPath: tr,
	})
	var stderr bytes.Buffer
	if code := SimplifyAfterEdits(strings.NewReader(payload), &stderr); code != ExitProceed {
		t.Errorf("meta.json agentType=Explore fallback must proceed, got %d: %s", code, stderr.String())
	}
}

// The gate must key off agent_transcript_path (the SUBAGENT's transcript), never transcript_path (the
// PARENT session's) — an edit that only appears in the parent transcript must never trigger the block.
func TestSimplifyAfterEditsParentTranscriptEditsIgnored(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := t.TempDir()
	parentTranscript := filepath.Join(dir, "parent.jsonl")
	if err := os.WriteFile(parentTranscript, []byte(editToolUseLine(t, "", "Edit", "/r/parent-edit.go")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The subagent's own transcript has no edits at all.
	subTranscript := writeTranscript(t, dir)
	payload := buildSubagentStopPayload(t, subagentStopFields{
		agentID: "a1", agentType: "general-purpose",
		transcriptPath: parentTranscript, agentTranscriptPath: subTranscript,
	})
	var stderr bytes.Buffer
	if code := SimplifyAfterEdits(strings.NewReader(payload), &stderr); code != ExitProceed {
		t.Errorf("edits in the parent transcript alone must not trigger the gate, got %d: %s", code, stderr.String())
	}
}
