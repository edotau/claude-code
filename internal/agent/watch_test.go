package agent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

var watchT0 = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

// watchRec is a job started age before watchT0 whose log was last written silence before it.
func watchRec(t *testing.T, status, prompt string, age, silence time.Duration) Record {
	t.Helper()
	log := filepath.Join(t.TempDir(), "job.log")
	if err := os.WriteFile(log, []byte("start\nworking on it\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	at := watchT0.Add(-silence)
	if err := os.Chtimes(log, at, at); err != nil {
		t.Fatal(err)
	}
	id := recordID(watchT0.Add(-age), "codex")
	return Record{ID: id, Time: watchT0.Add(-age), Agent: "codex", Status: status, Log: log, PID: 4242, Owner: 77,
		Request: RunOptions{Prompt: prompt, Tools: true, WorkDir: "/w"}}
}

// stubAlive makes every job's process alive (true) or gone (false) for the test.
func stubAlive(t *testing.T, alive bool) {
	t.Helper()
	old := watchAlive
	watchAlive = func(Record) bool { return alive }
	t.Cleanup(func() { watchAlive = old })
}

func marker(t *testing.T, kind, id, body string) {
	t.Helper()
	p := WatchMarker(kind, id)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClassifyRunningJobs(t *testing.T) {
	lim := WatchLimits{Age: 10 * time.Minute, Silence: 5 * time.Minute}
	cases := []struct {
		name, prompt string
		alive        bool
		age, silence time.Duration
		rel          relayState
		want         string
	}{
		{"healthy", "TASK ID: a", true, 12 * time.Minute, time.Minute, relayState{}, ""},
		{"silent past both limits", "TASK ID: a", true, 12 * time.Minute, 6 * time.Minute, relayState{}, WatchSilent},
		{"silent but young", "TASK ID: a", true, 8 * time.Minute, 6 * time.Minute, relayState{}, ""},
		{"fast tier halves the age", "Tier: fast", true, 6 * time.Minute, 6 * time.Minute, relayState{}, WatchSilent},
		{"standard tier keeps it", "Tier: standard", true, 6 * time.Minute, 6 * time.Minute, relayState{}, ""},
		{"deep tier doubles it", "Tier: deep", true, 15 * time.Minute, 6 * time.Minute, relayState{}, ""},
		{"deep tier past 20m", "Tier: deep", true, 21 * time.Minute, 6 * time.Minute, relayState{}, WatchSilent},
		{"dead at once", "x", false, time.Minute, 0, relayState{}, WatchDead},
		{"relay finished", "x", true, time.Minute, 0, relayState{finished: true}, WatchRelayGone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			stubAlive(t, c.alive)
			got, _ := classifyJob(watchRec(t, "running", c.prompt, c.age, c.silence), watchT0, lim, c.rel)
			if got.Reason != c.want {
				t.Fatalf("reason %q, want %q", got.Reason, c.want)
			}
		})
	}
}

func TestDeadJobNextCommand(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	stubAlive(t, false)
	r := watchRec(t, "running", "x", time.Minute, 0)
	got, _ := classifyJob(r, watchT0, WatchLimits{Age: time.Hour, Silence: time.Hour}, relayState{})
	if got.Resumable() || !slices.Equal(got.Next, []string{"claude-code", "agents", "result", r.ID}) {
		t.Fatalf("no session: next %v resumable %v, want agents result", got.Next, got.Resumable())
	}
	r.SessionID = "ses-1"
	got, _ = classifyJob(r, watchT0, WatchLimits{Age: time.Hour, Silence: time.Hour}, relayState{})
	want := "claude-code agents ask --agent codex --session ses-1 --background --tools --workdir /w " + ResumePrompt
	if !got.Resumable() || strings.Join(got.Next, " ") != want {
		t.Fatalf("next %q, want %q", strings.Join(got.Next, " "), want)
	}
	marker(t, "resumed", r.ID, "20261116T120100.000-codex\n")
	if got, _ = classifyJob(r, watchT0, WatchLimits{}, relayState{}); got.Reason != "" {
		t.Fatalf("a resumed dead job is %q, want ok", got.Reason)
	}
}

func TestClassifyFinishedJobs(t *testing.T) {
	ended := func(t *testing.T, ago time.Duration) Record {
		r := watchRec(t, "ok", "x", ago+time.Minute, ago)
		r.DurationMs = time.Minute.Milliseconds()
		return r
	}
	foreground := func(r Record) Record { r.PID = 0; return r }
	quickError := func(r Record) Record { r.Status, r.DurationMs = "error", 2000; return r }
	cases := []struct {
		name    string
		ago     time.Duration
		collect bool
		rel     relayState
		want    string
		pending bool
		edit    func(Record) Record
	}{
		{"uncollected past 5m", 10 * time.Minute, false, relayState{}, WatchUncollected, false, nil},
		{"just ended", 3 * time.Minute, false, relayState{}, "", true, nil},
		{"collected", 10 * time.Minute, true, relayState{}, "", false, nil},
		{"relay handed back after it ended", 10 * time.Minute, false, relayState{finished: true, at: watchT0}, "", false, nil},
		{"relay finished before it ended", 10 * time.Minute, false,
			relayState{finished: true, at: watchT0.Add(-time.Hour)}, WatchUncollected, false, nil},
		{"past the horizon", 3 * time.Hour, false, relayState{}, "", false, nil},
		{"background ask 6m old", 6 * time.Minute, false, relayState{}, WatchUncollected, false, nil},
		{"foreground ask 30m old", 30 * time.Minute, false, relayState{}, "", false, foreground},
		{"quick error", 30 * time.Minute, false, relayState{}, "", false, quickError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			r := ended(t, c.ago)
			if c.edit != nil {
				r = c.edit(r)
			}
			if c.collect {
				marker(t, "collected", r.ID, "")
			}
			got, pending := classifyJob(r, watchT0, WatchLimits{Age: time.Hour, Silence: time.Hour}, c.rel)
			if got.Reason != c.want || pending != c.pending {
				t.Fatalf("reason %q pending %v, want %q %v", got.Reason, pending, c.want, c.pending)
			}
		})
	}
}

func TestOwnedJobsFollowResumes(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	orig := Record{ID: "20261116T110000.000-codex", Owner: 77}
	resumed := Record{ID: "20261116T110500.000-codex", Owner: 9001} // launched by the hook process
	other := Record{ID: "20261116T111000.000-codex", Owner: 9002}
	marker(t, "resumed", orig.ID, resumed.ID+"\n")
	got := ownedJobs([]Record{other, resumed, orig}, 77) // newest first, as ReadRecent returns them
	if len(got) != 2 || got[0].ID != orig.ID || got[1].ID != resumed.ID {
		t.Fatalf("owned %v, want the original and its resume", got)
	}
}

func TestLastLogLineDropsCredentials(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.log")
	for body, want := range map[string]string{
		"a\nediting watch.go\n\n":                       "editing watch.go",
		"a\ntoken sk-" + strings.Repeat("A", 24) + "\n": "",
	} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := lastLogLine(p); got != want {
			t.Fatalf("lastLogLine(%q) = %q, want %q", body, got, want)
		}
	}
}

func TestScanRelaysFindsTheLaunchingTranscript(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	dir := filepath.Join(cfg, "projects", "p", "sess", "subagents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	r := Record{ID: "20261116T110000.000-codex", Time: time.Now().Add(-time.Minute)}
	if err := os.WriteFile(filepath.Join(dir, "agent-a1.jsonl"), []byte("job "+r.ID+" started\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent-a1.meta.json"), []byte(`{"finished":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := scanRelays([]Record{r}); !got[r.ID].finished {
		t.Fatalf("relay state %+v, want finished", got[r.ID])
	}
}

func TestWatchLinesFit80Columns(t *testing.T) {
	s := StaleJob{ID: "20261116T110000.000-opencode", Task: strings.Repeat("long task ", 12), Agent: "opencode",
		Model: "databricks-claude-sonnet-4-6", Reason: WatchDead, Age: 90 * time.Minute, Silence: time.Hour,
		LastLine: strings.Repeat("x", 120), Next: ResumeArgs(Record{Agent: "opencode",
			Request: RunOptions{Tools: true, WorkDir: "/Users/someone/worktrees/repo/slug"}}, "ses_0123456789abcdef")}
	lines := WatchLines(s, 80)
	for _, l := range lines {
		if len([]rune(l)) > 80 {
			t.Fatalf("line over 80 columns (%d): %q", len([]rune(l)), l)
		}
	}
	if joined := strings.Join(lines, "\n"); !strings.Contains(joined, "--session ses_0123456789abcdef") {
		t.Fatalf("next command missing:\n%s", joined)
	}
}
