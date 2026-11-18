package inspect

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/proc"
)

type dirtyEngine struct{ act func() }

func (*dirtyEngine) Name() string     { return "fake" }
func (*dirtyEngine) Caps() agent.Caps { return agent.Caps{} }
func (*dirtyEngine) Available() error { return nil }
func (e *dirtyEngine) Run(context.Context, agent.RunOptions, wire.Sink) (wire.Result, error) {
	e.act()
	return wire.Result{Answer: "done"}, nil
}

func dirtyDispatch(t *testing.T, dir string) string {
	t.Helper()
	write := func(p, body string, stamp int64) {
		t.Helper()
		writeRepoFile(t, dir, p, body)
		ts := time.Unix(stamp, 0)
		if err := os.Chtimes(filepath.Join(dir, p), ts, ts); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"edited.txt", "idle.txt", "owned.txt", "reverted.txt"} {
		write(p, "base\n", 1)
	}
	repoGit(t, dir, "add", "edited.txt", "idle.txt", "owned.txt", "reverted.txt")
	repoGit(t, dir, "commit", "-qm", "fixture")
	for _, p := range []string{"edited.txt", "idle.txt", "owned.txt", "reverted.txt"} {
		write(p, "dirt\n", 2)
	}
	engine := &dirtyEngine{act: func() {
		write("edited.txt", "edit\n", 3)
		write("owned.txt", "edit\n", 3)
		write("reverted.txt", "base\n", 3)
	}}
	req := agent.RunOptions{Agent: "fake", Prompt: "fixture", WorkDir: dir}
	p := &agent.Prepared{Opts: req, Record: agent.ClaimRecord("fake", req, time.Now()), Chain: agent.Single(engine)}
	if _, err := (&agent.Client{}).Exec(context.Background(), p, wire.Discard, agent.RunRecordedOpts{}); err != nil {
		t.Fatal(err)
	}
	return p.Record.ID
}

func TestDiffFlagsPreDirtyOutside(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(proc.DispatchIDEnv, "")
	dir := newDiffRepo(t)
	id := dirtyDispatch(t, dir)
	for _, verify := range []string{id, ""} {
		t.Setenv(proc.DispatchIDEnv, id)
		code, out, errOut := runDiff(t, DiffOptions{Dir: dir, Verify: verify, Paths: []string{"owned.txt"}})
		if code != DiffExitOutside || errOut != "" {
			t.Fatalf("exit %d stderr %q\n%s", code, errOut, out)
		}
		for _, p := range []string{"edited.txt", "reverted.txt"} {
			wantInOutput(t, out, p+": was already modified before dispatch "+id+" started and is outside --path")
		}
		if strings.Count(out, ": was already modified before dispatch ") != 2 {
			t.Fatalf("unexpected warnings:\n%s", out)
		}
	}
	repoGit(t, dir, "add", "edited.txt", "idle.txt", "owned.txt")
	repoGit(t, dir, "commit", "-qm", "keep other edits")
	code, out, _ := runDiff(t, DiffOptions{Dir: dir, Verify: id, Paths: []string{"edited.txt", "idle.txt", "owned.txt"}})
	if code != DiffExitOutside {
		t.Fatalf("reverted file alone: exit %d\n%s", code, out)
	}
	wantInOutput(t, out, "outside --path (1):\n  reverted.txt: was already modified")
}
