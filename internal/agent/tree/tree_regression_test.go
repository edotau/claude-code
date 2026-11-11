package tree

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildRejectsSessionGlobPatterns(t *testing.T) {
	claudeDir, now := copyTestdataTree(t)
	for _, id := range []string{"*", "?", "[", "[rs]*", `running\-session`} {
		t.Run(id, func(t *testing.T) {
			got, err := build(claudeDir, Options{Session: id, Days: 1, Now: pinNow(now)})
			if err == nil {
				t.Fatalf("build(Session=%q) = %+v, want rejection instead of glob expansion", id, got)
			}
			if !strings.Contains(err.Error(), "not a plain session id") {
				t.Errorf("build(Session=%q) error = %v, want plain-session validation", id, err)
			}
		})
	}
}

func TestBackgroundKeepsLiveSessionWithOldTranscript(t *testing.T) {
	claudeDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	regFixture(t, claudeDir, "live-old", os.Getpid(), 40*24*time.Hour, now)
	regFixture(t, claudeDir, "dead-old", deadPID, 40*24*time.Hour, now)
	for _, running := range []bool{true, false} {
		got, err := listing(claudeDir, Options{Days: 1, Running: running, Now: pinNow(now)}, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Sessions) != 1 {
			t.Fatalf("listing(Running=%t) sessions = %+v, want only the old live session", running, got.Sessions)
		}
		s := got.Sessions[0]
		if s.ID != "live-old" || !s.Live || s.PID != os.Getpid() {
			t.Errorf("listing(Running=%t) session = %+v, want the live registered session", running, s)
		}
	}
}

func TestBuildRunningKeepsStaleOnlySession(t *testing.T) {
	for _, kind := range []string{"direct", "workflow"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", dir)
			now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
			project := filepath.Join(dir, "projects", "-repo")
			writeFixFixture(t, filepath.Join(project, "idle.jsonl"), `{"type":"user","cwd":"/repo","message":{"content":"hello"}}`)
			agents := filepath.Join(project, "idle", "subagents")
			if kind == "workflow" {
				agents = filepath.Join(agents, "workflows", "wf-stale")
			}
			writeFixFixture(t, filepath.Join(agents, "agent-stale.jsonl"), "")
			chtimesAll(t, project, now.Add(-time.Hour))
			got, err := build(dir, Options{Running: true, Now: pinNow(now)})
			if err != nil {
				t.Fatal(err)
			}
			s := sessionByID(t, got, "idle")
			if s.Live || s.Status != statusIdle {
				t.Fatalf("session = %+v, want non-live idle session", s)
			}
			subs := s.Subagents
			if kind == "workflow" {
				subs = workflowByID(t, s.Workflows, "wf-stale").Phases[0].Agents
			}
			if len(subs) != 1 || subs[0].ID != "stale" || subs[0].Status != statusStale {
				t.Fatalf("subagents = %+v, want the unfinished stale agent retained", subs)
			}
		})
	}
}

func writeFixFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Regression: the pre-change JSON schema (lastTool, no activity/dispatches) still decodes a tree built
// after the change — new fields are additive (omitempty), never required.
func TestJSONSchemaStillDecodesPreChangeShape(t *testing.T) {
	claudeDir, now := copyTestdataTree(t)
	tree, err := build(claudeDir, Options{Session: "finished-session-aaaa1111", Now: pinNow(now)})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(tree)
	if err != nil {
		t.Fatal(err)
	}
	var old struct {
		Sessions []struct {
			Subagents []struct {
				ID       string `json:"id"`
				Status   string `json:"status"`
				LastTool string `json:"lastTool"`
			} `json:"subagents"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &old); err != nil {
		t.Fatalf("pre-change schema failed to decode post-change JSON: %v\nraw: %s", err, raw)
	}
	if len(old.Sessions) != 1 || len(old.Sessions[0].Subagents) != 2 {
		t.Fatalf("decoded = %+v", old)
	}
}
