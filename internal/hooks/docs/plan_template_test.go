package docs

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
)

func writePayload(t *testing.T, tool, cwd, path, content string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"hook_event_name": "PreToolUse", "tool_name": tool, "cwd": cwd,
		"tool_input": map[string]string{"file_path": path, "content": content},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestPlanTemplateTeeth proves the gate actually fires: today (no gate consulted) a free-form first write to
// an undated draft passes; with PlanTemplate wired in, the same write is denied, and the message names plan.md.
func TestPlanTemplateTeeth(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if err := os.MkdirAll(filepath.Join(cfg, "plans"), 0o700); err != nil {
		t.Fatal(err)
	}
	draft := filepath.Join(cfg, "plans", "funny-random-words.md")

	var stderr bytes.Buffer
	code := Gate(core.ParseInput(strings.NewReader(writePayload(t, "Write", cfg, draft, "just some free-form draft text\n"))), &stderr)
	if code != core.ExitBlock {
		t.Fatalf("free-form first write: PlanTemplate = %d, want ExitBlock", code)
	}
	if !strings.Contains(stderr.String(), "plan-template") || !strings.Contains(stderr.String(), "plan.md") {
		t.Fatalf("denial message = %q, want it to name plan-template and plan.md", &stderr)
	}
	if _, err := os.Stat(draft); err == nil {
		t.Fatalf("a denied write must not have landed on disk")
	}
}

func TestPlanTemplateAllowsTemplatedFirstWrite(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if err := os.MkdirAll(filepath.Join(cfg, "plans"), 0o700); err != nil {
		t.Fatal(err)
	}
	draft := filepath.Join(cfg, "plans", "funny-random-words.md")
	code := Gate(core.ParseInput(strings.NewReader(writePayload(t, "Write", cfg, draft, planTemplate))), &bytes.Buffer{})
	if code != core.ExitProceed {
		t.Fatalf("templated first write: PlanTemplate = %d, want ExitProceed", code)
	}
}

// TestPlanTemplateOnlyGatesTheFirstWrite: once the draft exists on disk, a later Write to it (e.g. the
// model rewriting its own draft) is not re-gated, even with content missing the headings.
func TestPlanTemplateOnlyGatesTheFirstWrite(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if err := os.MkdirAll(filepath.Join(cfg, "plans"), 0o700); err != nil {
		t.Fatal(err)
	}
	draft := filepath.Join(cfg, "plans", "funny-random-words.md")
	if err := os.WriteFile(draft, []byte(planTemplate), 0o600); err != nil {
		t.Fatal(err)
	}
	code := Gate(core.ParseInput(strings.NewReader(writePayload(t, "Write", cfg, draft, "no headings now\n"))), &bytes.Buffer{})
	if code != core.ExitProceed {
		t.Fatalf("second write to an existing draft: PlanTemplate = %d, want ExitProceed", code)
	}
}

// TestPlanTemplateAllFourHeadingsPresent: the exact template content passes with all 4 headings as plain text.
func TestPlanTemplateAllFourHeadingsPresent(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if err := os.MkdirAll(filepath.Join(cfg, "plans"), 0o700); err != nil {
		t.Fatal(err)
	}
	draft := filepath.Join(cfg, "plans", "all-headings.md")
	content := "# Plan: Z\n\n## Context\n\nwhy\n\n## Phases\n\n### Phase 1\n\n## Tasks\n\n- [ ] Phase 1\n"
	code := Gate(core.ParseInput(strings.NewReader(writePayload(t, "Write", cfg, draft, content))), &bytes.Buffer{})
	if code != core.ExitProceed {
		t.Fatalf("all 4 headings present: Gate = %d, want ExitProceed", code)
	}
}

// TestPlanTemplateRejectsFencedOnlyHeading: Gate now parses the mdsmith AST, so a heading that
// only appears inside a fenced code block does not count as present (fixed from a strings.Contains scan,
// which previously let this through as ExitProceed).
func TestPlanTemplateRejectsFencedOnlyHeading(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if err := os.MkdirAll(filepath.Join(cfg, "plans"), 0o700); err != nil {
		t.Fatal(err)
	}
	draft := filepath.Join(cfg, "plans", "fenced-heading.md")
	// "## Tasks" only appears inside the fence below, never as a real heading.
	content := "# Plan: Z\n\n## Context\n\nwhy\n\n## Phases\n\n```\n## Tasks\n```\n"
	var stderr bytes.Buffer
	code := Gate(core.ParseInput(strings.NewReader(writePayload(t, "Write", cfg, draft, content))), &stderr)
	if code != core.ExitBlock {
		t.Fatalf("fenced-only heading = %d, want ExitBlock (fence content must not satisfy the template check)", code)
	}
	if !strings.Contains(stderr.String(), "## Tasks") {
		t.Fatalf("denial message = %q, want it to name the missing heading", &stderr)
	}
}

func TestPlanTemplateIgnoresDatedAndOutsidePlans(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	for _, path := range []string{
		filepath.Join(cfg, "plans", "2026-10-20-already-dated.md"),
		filepath.Join(cfg, "plans", "sub", "funny-random-words.md"),
		filepath.Join(t.TempDir(), "elsewhere.md"),
	} {
		code := Gate(core.ParseInput(strings.NewReader(writePayload(t, "Write", cfg, path, "no headings\n"))), &bytes.Buffer{})
		if code != core.ExitProceed {
			t.Errorf("PlanTemplate(%s) = %d, want ExitProceed (not an undated top-level draft)", path, code)
		}
	}
}
