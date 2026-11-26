package plans

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/platform/hosttest"
)

const doneBase = "- [x] Phase 1.1 — base"

// readyFixture writes one plan opening with first, whose 1.2 waits on 1.1, a prose-blocked 1.3, an owner-gated 1.4.
func readyFixture(t *testing.T, first string) ReadyInput {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	root := hosttest.Repo(t)
	plan := filepath.Join(t.TempDir(), "2026-11-26-ready.md")
	body := first + " — leaf: claude — verify: go test ./a — owns: a/a.go\n" +
		"- [ ] Phase 1.2 — next — leaf: codex — verify: go test ./b — Blocked by: 1.1 — owns: b/b.go\n" +
		"- [ ] Phase 1.3 — lane — leaf: claude — Blocked by: plan C's X lane landing — owns: c/c.go\n" +
		"- [ ] Phase 1.4 — gated (owner decision first) — leaf: claude — owns: d/d.go\n"
	if err := os.WriteFile(plan, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return ReadyInput{Root: root, Plans: []string{plan}, Slots: 2, Claims: func() map[string][]string { return nil }}
}

func reasons(rep ReadyReport) map[string]string {
	out := map[string]string{}
	for _, r := range append(slices.Clone(rep.Ready), rep.NotReady...) {
		out[r.ID] = r.Reason
	}
	return out
}

func TestReadyBlockers(t *testing.T) {
	for _, tc := range []struct{ first, want12 string }{
		{"- [ ] Phase 1.1 — base", "blocked by Phase 1.1 (open)"},
		{doneBase, ""},
		{"- [ ] ~~Phase 1.1 — base~~", ""},
	} {
		rep, err := Ready(readyFixture(t, tc.first))
		if err != nil {
			t.Fatal(err)
		}
		got := reasons(rep)
		if got["Phase 1.2"] != tc.want12 {
			t.Errorf("%s: 1.2 reason %q, want %q", tc.first, got["Phase 1.2"], tc.want12)
		}
		if !strings.HasPrefix(got["Phase 1.3"], "prose blocker: plan C's X lane landing") {
			t.Errorf("1.3 reason %q, want the prose blocker", got["Phase 1.3"])
		}
		if !strings.HasPrefix(got["Phase 1.4"], "waits on the owner") {
			t.Errorf("1.4 reason %q, want the owner gate", got["Phase 1.4"])
		}
	}
}

func TestReadyClaimedOwns(t *testing.T) {
	in := readyFixture(t, doneBase)
	file := filepath.Join(in.Root, "b", "b.go")
	writeFile(t, file, "package b\n") // untracked, so the claim on it is live
	in.Claims = func() map[string][]string { return map[string][]string{file: {"leaf abc"}} }
	if got := ready12(t, in); got != "owns claimed: b/b.go by leaf abc" {
		t.Fatalf("1.2 reason %q, want the claim", got)
	}
}

func TestReadyDispatchCommand(t *testing.T) {
	brief := "/tmp/b.md"
	for leaf, want := range map[string]string{
		"pi":     `claude-code agents ask --background --overlay '{"agent":"pi"}' - < '/tmp/b.md'`,
		"codex":  `claude-code agents ask --carry --background --overlay '{"agent":"codex","tools":true}' - < '/tmp/b.md'`,
		"claude": `claude-code agents ask --background --overlay '{"agent":"claude","tools":true}' - < '/tmp/b.md'`,
	} {
		if got := dispatchCommand(ReadyTask{Leaf: leaf, Brief: brief}); got != want {
			t.Errorf("%s: %q, want %q", leaf, got, want)
		}
	}
	if ids, ok := blockerIDs("(see plan B)", nil); ok || ids != nil {
		t.Errorf("a parenthetical-only blocker must stay prose")
	}
}

// A live claim on a clean file is released; an unlanded worktree-* commit claims the files it changed.
func TestReadyLaneAndStaleClaims(t *testing.T) {
	in := readyFixture(t, doneBase)
	file := filepath.Join(in.Root, "b", "b.go")
	writeFile(t, file, "package b\n")
	hosttest.In(t, in.Root, "add", "b/b.go")
	hosttest.In(t, in.Root, "commit", "-q", "-m", "b")
	in.Claims = func() map[string][]string { return map[string][]string{file: {"session s"}} }
	if got := ready12(t, in); got != "" {
		t.Fatalf("a claim on a clean file must be released, got %q", got)
	}
	writeFile(t, file, "package b // edit\n")
	if got := ready12(t, in); got != "owns claimed: b/b.go by session s" {
		t.Fatalf("a claim on a dirty file must hold, got %q", got)
	}
	hosttest.In(t, in.Root, "checkout", "-q", "-b", "worktree-x")
	hosttest.In(t, in.Root, "commit", "-q", "-am", "lane edit")
	hosttest.In(t, in.Root, "checkout", "-q", "main")
	in.Claims = nil
	if got := ready12(t, in); got != "" {
		t.Fatalf("a lane commit with no live edit claims nothing, got %q", got)
	}
}

// A lane file is released when its blob is already on HEAD or equals the lane's base, else it stays claimed.
func ready12(t *testing.T, in ReadyInput) string {
	t.Helper()
	rep, err := Ready(in)
	if err != nil {
		t.Fatal(err)
	}
	return reasons(rep)["Phase 1.2"]
}

func TestReadyJSON(t *testing.T) {
	rep, err := Ready(readyFixture(t, doneBase))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Slots    int              `json:"slots"`
		Ready    []map[string]any `json:"ready"`
		NotReady []map[string]any `json:"not_ready"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Slots != 2 || len(got.Ready) != 1 || len(got.NotReady) != 2 {
		t.Fatalf("shape %s", b)
	}
	want := map[string]any{"plan": rep.Ready[0].Plan, "id": "Phase 1.2", "leaf": "codex", "owns": []any{"b/b.go"},
		"verify": "go test ./b", "brief": NeedsBrief, "command": "claude-code agents brief --path codex"}
	if !reflect.DeepEqual(got.Ready[0], want) {
		t.Fatalf("ready row %v, want %v", got.Ready[0], want)
	}
}
