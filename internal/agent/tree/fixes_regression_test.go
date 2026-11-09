package tree

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/inflight"
)

func TestDropDispatchSessionsKeepsConflictingSessionID(t *testing.T) {
	start := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	dispatch := Dispatch{
		Agent: "claude", sessionID: "session-a", cwd: "/repo",
		started: start, prompt: "Summarize the retry logic",
	}
	sessions := []Session{
		{ID: "session-a", Cwd: dispatch.cwd, Title: dispatch.prompt, Updated: start.Add(time.Minute)},
		{ID: "session-b", Cwd: dispatch.cwd, Title: dispatch.prompt, Updated: start.Add(time.Minute)},
	}
	got := dropDispatchSessions(sessions, []Dispatch{dispatch})
	if len(got) != 1 || got[0].ID != "session-b" {
		t.Fatalf("sessions = %+v, want unrelated session-b retained despite matching title", got)
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
			got, err := Build(dir, Options{Running: true, Now: pinNow(now)})
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
			if len(subs) != 1 || subs[0].ID != "stale" || subs[0].Status != StatusStale {
				t.Fatalf("subagents = %+v, want the unfinished stale agent retained", subs)
			}
		})
	}
}

func TestReadSubagentMetaRejectsPartialDecode(t *testing.T) {
	transcript := filepath.Join(t.TempDir(), "agent-partial.jsonl")
	writeFixFixture(t, strings.TrimSuffix(transcript, ".jsonl")+".meta.json", `{"finished":true,"spawnDepth":"invalid"}`)
	if got := readSubagentMeta(transcript); got != (subagentMeta{}) {
		t.Fatalf("metadata = %+v, want zero value for invalid sidecar", got)
	}
}

func TestReapReportsMarkerDirectoryStatFailure(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := inflight.Dir()
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(dir), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err == nil || os.IsNotExist(err) {
		t.Fatalf("stat self-referential marker directory = %v, want non-missing error", err)
	}
	var out, errw bytes.Buffer
	if code := Reap(false, &out, &errw); code != 1 {
		t.Fatalf("Reap = %d, want 1 for marker directory stat failure; stdout=%q stderr=%q", code, &out, &errw)
	}
	if !strings.Contains(errw.String(), "marker dir:") {
		t.Fatalf("stderr = %q, want marker directory error", &errw)
	}
}

func TestWorkflowManifestNameUsesMeta(t *testing.T) {
	manifest := workflowManifest{Script: "const task = { name: 'worker' }; const meta = { name: 'release' };"}
	if got := manifest.name(); got != "release" {
		t.Fatalf("workflow name = %q, want meta.name release rather than task.name worker", got)
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
