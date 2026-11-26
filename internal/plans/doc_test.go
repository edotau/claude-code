package plans

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

// The serializer writes back exactly what it read, and "#N" names the Nth checklist item.
func TestDocRoundTripAndIndex(t *testing.T) {
	text := "# P\n\n- [ ] 1.1 One\n  more\n```\n- [ ] fenced\n```\n- [x] 1.2 Two (abc1234)\n"
	d := ParseDoc("p.md", []byte(text))
	if d.Render() != text {
		t.Fatalf("round trip changed the text:\n%s", d.Render())
	}
	for ref, want := range map[string]string{"#1": "1.1", "#2": "1.2", "1.2": "1.2"} {
		if got, ok := d.Find(ref); !ok || got.ID != want {
			t.Errorf("%s: got %q, %v; want %s", ref, got.ID, ok, want)
		}
	}
	for _, ref := range []string{"#0", "#3", "#01", "#x"} {
		if got, ok := d.Find(ref); ok {
			t.Errorf("%s resolved to %q", ref, got.ID)
		}
	}
}

// A lane mark is one comment on the task's last line: replaced, never stacked, and gone once the task ticks or strikes.
func TestMarkLaneReplacesAndTickDrops(t *testing.T) {
	hermetic(t)
	plan := filepath.Join(t.TempDir(), "2026-10-05-p.md")
	writeFile(t, plan, "- [ ] 1.1 One\n  owns: a.go\n- [ ] 1.2 Two\n")
	for _, sha := range []string{"abc1234def", "def5678abc"} {
		if wrote, err := MarkLane(plan, "#1", "lane-a", sha); !wrote || err != nil {
			t.Fatalf("mark %s: %v %v", sha, wrote, err)
		}
	}
	want := "- [ ] 1.1 One\n  owns: a.go <!-- lane lane-a: def5678 -->\n- [ ] 1.2 Two\n"
	if got := readFile(t, plan); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if wrote, _ := MarkLane(plan, "1.1", "lane-a", "def5678abc"); wrote {
		t.Fatal("the same mark wrote again")
	}
	if _, err := MarkLane(plan, "1.1", "two words", "def5678"); err == nil {
		t.Fatal("a lane with a space was accepted")
	}
	s := summarize(ParseDoc(plan, []byte(readFile(t, plan))).Plan)
	if s.Total != 2 || s.Ticked != 0 || len(s.Lanes) != 1 || s.Lanes[0] != "1.1: in lane lane-a (def5678)" {
		t.Fatalf("summary %+v", s)
	}
	if _, err := MarkDone(plan, "1.1", "fedcba9"); err != nil {
		t.Fatal(err)
	}
	if _, err := MarkLane(plan, "1.2", "lane-b", "1234567"); err != nil {
		t.Fatal(err)
	}
	if _, err := StrikeWith(plan, "1.2", "obsolete", "note:x"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, plan); strings.Contains(got, "lane") || !strings.Contains(got, "[x] 1.1") || !strings.Contains(got, "~~") {
		t.Fatalf("tick and strike must drop the lane mark:\n%s", got)
	}
}

// A lane commit naming a task marks it in that lane with its sha; the main commit that lands it ticks it.
func TestLaneCommitMarksThenMainTicks(t *testing.T) {
	hermetic(t)
	repo := gitRepo(t, "main")
	plan := filepath.Join(Home(repo), "2026-10-05-p.md")
	writeFile(t, plan, "- [ ] 1.1 Lane task owns: a.go\n- [ ] 1.2 Other owns: b.go\n")
	lane := filepath.Join(t.TempDir(), "lane-x")
	hosttest.In(t, repo, "worktree", "add", "-q", "-b", "lane-x", lane)
	hosttest.WriteRel(t, lane, "a.go", "package a\n")
	hosttest.In(t, lane, "add", "a.go")
	hosttest.In(t, lane, "commit", "-q", "-m", "feat: a\n\nTask: 2026-10-05-p 1.1")
	sha := strings.TrimSpace(hosttest.In(t, lane, "rev-parse", "HEAD"))
	pending, _, err := PendingCommit(lane, sha, nil)
	if err != nil || len(pending) != 1 || pending[0].Lane != "lane-x" || strings.Join(pending[0].IDs, ",") != "1.1" {
		t.Fatalf("pending %+v, %v", pending, err)
	}
	if readFile(t, plan) != "- [ ] 1.1 Lane task owns: a.go\n- [ ] 1.2 Other owns: b.go\n" {
		t.Fatal("the dry run wrote")
	}
	got, _, err := TickCommit(lane, sha, nil, nil) // what the verifier does when green
	if err != nil || len(got) != 1 || !strings.Contains(strings.Join(got[0].Lines(sha), "\n"), "1.1 marked in lane lane-x ("+sha[:7]+")") {
		t.Fatalf("lane tick %+v, %v", got, err)
	}
	want := "- [ ] 1.1 Lane task owns: a.go <!-- lane lane-x: " + sha[:7] + " -->\n- [ ] 1.2 Other owns: b.go\n"
	if readFile(t, plan) != want {
		t.Fatalf("got %q, want %q", readFile(t, plan), want)
	}
	hosttest.In(t, repo, "merge", "-q", "--ff-only", "lane-x")
	if _, _, err := TickCommit(repo, sha, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, plan); got != "- [x] 1.1 Lane task owns: a.go ("+sha[:7]+")\n- [ ] 1.2 Other owns: b.go\n" {
		t.Fatalf("main tick must replace the mark with the sha: %q", got)
	}
}

// A closed line without leaf: or an older malformed open line never blocks an add; a new break still refuses.
func TestAddTaskBesideMalformedLines(t *testing.T) {
	closed := "## Tasks\n\n- [x] 7.1 vendors tasks — owns: the report's files\n"
	file := wavesFile(t, closed)
	notes, err := AddTask(file, WaveTask{ID: "Phase 7.9", Task: "New", Leaf: "claude", Agent: "pi", Owns: []string{"b.go"}})
	if err != nil || len(notes) != 0 || !strings.Contains(readFile(t, file), "Phase 7.9 — New") {
		t.Fatalf("closed line blocked the add: %v %v", notes, err)
	}
	broken := "## Tasks\n\n- [ ] Phase 7.2 x — owns: c.go\n- [ ] Phase 7.3 y — leaf: claude, Agent: pi — owns: b.go\n"
	file = wavesFile(t, broken)
	notes, err = AddTask(file, WaveTask{ID: "Phase 7.9", Task: "New", Leaf: "claude", Agent: "pi", Owns: []string{"d.go"}})
	if err != nil || len(notes) != 1 || !strings.Contains(notes[0], "Phase 7.2: missing leaf:") {
		t.Fatalf("an old malformed line blocked the add: %v %v", notes, err)
	}
	file = wavesFile(t, broken)
	_, err = AddTask(file, WaveTask{ID: "Phase 7.9", Task: "New", Leaf: "claude", Agent: "pi", Owns: []string{"b.go"}})
	if err == nil || !strings.Contains(err.Error(), "overlaps Phase 7.3") || readFile(t, file) != broken {
		t.Fatalf("the old error hid the new overlap: %v", err)
	}
}
