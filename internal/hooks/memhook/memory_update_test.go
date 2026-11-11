package memhook

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/worktree"
)

func updatePayload(tr, cwd string) string {
	return fmt.Sprintf(`{"session_id":"s1","cwd":%q,"transcript_path":%q}`, cwd, tr)
}

// hookSession is a hermetic session whose transcript the caller fills; payload is its Stop/End/Compact input.
func hookSession(t *testing.T) (tr string, payload func() *core.Input) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(memory.UpdateChildEnv, "")
	tr, cwd := filepath.Join(t.TempDir(), "t.jsonl"), t.TempDir()
	return tr, func() *core.Input { return core.ParseInput(strings.NewReader(updatePayload(tr, cwd))) }
}

// A failed Stop spawn blocks with the live work units, never mislabeled as file edits.
func TestFailedWorkerMessageUsesWorkUnits(t *testing.T) {
	tr, payload := hookSession(t)
	hosttest.WriteUserTurns(t, tr, 6)
	memory.StubSpawn(t, false)
	for range 2 {
		r := Stop(payload())
		if r.Decision != core.Block || !strings.Contains(r.Reason, "~3 work units this session") {
			t.Fatalf("six user turns and zero edits: %+v, want block with 3 work units", r)
		}
		if !strings.Contains(r.Reason, "claude-code memory update") || strings.Contains(r.Reason, "file edits") {
			t.Fatalf("block reason %q", r.Reason)
		}
	}
}

// At the block cap Stop fails open once with the not-saved notice; later Stops stay quiet.
func TestStopFailOpenSaysUpdateNotSaved(t *testing.T) {
	tr, payload := hookSession(t)
	hosttest.WriteEdits(t, tr, 5)
	memory.StubSpawn(t, false)
	for range 3 {
		Stop(payload())
	}
	if r := Stop(payload()); r.Decision == core.Block || r.SystemMessage != updateNotSaved {
		t.Fatalf("Stop at the cap = %+v, want proceed with %q", r, updateNotSaved)
	}
	if r := Stop(payload()); r.Decision == core.Block || r.SystemMessage != "" {
		t.Fatalf("Stop past the cap = %+v, want a quiet proceed", r)
	}
}

func TestEndAndCompactDispatchOnce(t *testing.T) {
	for name, hook := range map[string]func(*core.Input) core.Result{"End": End, "Compact": Compact} {
		t.Run(name, func(t *testing.T) {
			tr, payload := hookSession(t)
			hosttest.WriteEdits(t, tr, 3)
			spawns := memory.StubSpawn(t, true)
			for range 2 {
				if r := hook(payload()); !reflect.DeepEqual(r, core.Result{}) {
					t.Fatalf("%s = %+v, want an empty result", name, r)
				}
			}
			if got := spawns.Update.Load(); got != 1 {
				t.Fatalf("%s dispatched %d updates, want 1", name, got)
			}
		})
	}
}

// An update in a linked worktree dispatches the background worker and logs one ledger line.
func TestUpdateAppendsWorktreeLedger(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", home+"/.claude")
	t.Setenv(memory.UpdateChildEnv, "")
	main := filepath.Join(home, "repo")
	for _, a := range [][]string{{"init", "-q", "-b", "main", main}, {"-C", main, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "--allow-empty", "-q", "-m", "i"}} {
		if out, err := exec.Command("git", a...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", a, err, out)
		}
	}
	repo, err := worktree.ResolveRepo(main)
	if err != nil {
		t.Fatal(err)
	}
	dest, err := worktree.Create(repo, "wt-h", worktree.CreateOptions{Log: &bytes.Buffer{}})
	if err != nil {
		t.Fatal(err)
	}
	spawns := memory.StubSpawn(t, true)
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	hosttest.WriteEdits(t, tr, 3)
	if res := Stop(core.ParseInput(strings.NewReader(updatePayload(tr, dest)))); res.Decision == core.Block || spawns.Update.Load() != 1 {
		t.Fatalf("update = %+v, spawns=%d; want not blocked and one background worker", res, spawns.Update.Load())
	}
	data, err := os.ReadFile(filepath.Join(memory.Dir(main), "worktrees", "wt-h.md"))
	if err != nil || !strings.Contains(string(data), "update (") || !strings.Contains(string(data), "3 edits, session s1") {
		t.Fatalf("no update line in ledger: %v\n%s", err, data)
	}
}
