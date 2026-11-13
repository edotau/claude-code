package memory

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
)

const reviseLog = `# Decision Log

## 2026-11-13 — Audit executes tests serially <!-- id:b0000002 -->
Supersedes a0000001. Tests now execute serially.

## 2026-11-13 — Names a missing entry <!-- id:c0000003 -->
Supersedes deadbeef. Unrelated gateway note.

## 2026-11-12 — First of a cycle <!-- id:d0000004 -->
Supersedes e0000005. Gateway cache note.

## 2026-11-12 — Second of a cycle <!-- id:e0000005 -->
Supersedes d0000004. Gateway cache note.

## 2026-11-12 — Keep the old shard layout for audit tests <!-- id:a0000001 -->
Shard layout audit tests run concurrently.
`

// TestSettleStrikesSupersededAndRecallFollows: settle strikes what a newer entry supersedes; recall serves the newer.
func TestSettleStrikesSupersededAndRecallFollows(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	path := Dir(root) + "/decisionLog.md"
	write(t, path, reviseLog)
	if _, err := settleBank(root, time.Now()); err != nil {
		t.Fatal(err)
	}
	got := mustRead(t, path)
	want := "~~Keep the old shard layout for audit tests~~ <!-- id:a0000001 -->\nReplaced by b0000002 (2026-11-13)."
	if !strings.Contains(got, want) {
		t.Fatalf("old entry not struck with its pointer and id kept; want %q in:\n%s", want, got)
	}
	assertUnmarked(t, got, "c0000003", "d0000004", "e0000005") // a dangling reference and a cycle
	if _, err := settleBank(root, time.Now()); err != nil || mustRead(t, path) != got {
		t.Fatalf("second settle changed the file (err %v)", err)
	}
	assertRecallFollows(t, root)
}

// assertRecallFollows: recall serves the new entry for the old one's terms; review reports the two bad refs.
func assertRecallFollows(t *testing.T, root string) {
	t.Helper()
	picks, _ := recallPicks(root, "", "shard layout audit run concurrently", false, false)
	if len(picks) != 1 || picks[0].Entry.ID != "b0000002" {
		t.Fatalf("recall = %+v; want only the replacing entry b0000002", picks)
	}
	if p := reviseProblems(root); len(p) != 2 || !strings.Contains(strings.Join(p, "\n"), "deadbeef") {
		t.Fatalf("review problems = %q; want the dangling id and the cycle", p)
	}
}

// assertUnmarked fails when any of ids has a strike or a pointer in log.
func assertUnmarked(t *testing.T, log string, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if b := blockOf(t, log, id); strings.Contains(b, "~~") || replacedBy(b) != "" {
			t.Errorf("%s was marked", id)
		}
	}
}

// blockOf is the block of the decision log text carrying id.
func blockOf(t *testing.T, log, id string) string {
	t.Helper()
	_, blocks := splitHeadingBlocks(log)
	for _, b := range blocks {
		if ownerID(b) == id {
			return b
		}
	}
	t.Fatalf("no block %s", id)
	return ""
}

// TestReviseEntry: dry-run writes nothing; a real run links both entries in one commit; a repeat is a no-op.
func TestReviseEntry(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	path := Dir(root) + "/decisionLog.md"
	plain := strings.ReplaceAll(reviseLog, "Supersedes a0000001. ", "")
	write(t, path, plain)
	if _, err := ReviseEntry(root, "a0000001", "b0000002", true); err != nil || mustRead(t, path) != plain {
		t.Fatalf("dry run wrote or failed: %v", err)
	}
	if _, err := ReviseEntry(root, "a0000001", "b0000002", false); err != nil {
		t.Fatal(err)
	}
	got := mustRead(t, path)
	if replacedBy(blockOf(t, got, "a0000001")) != "b0000002" || !strings.Contains(blockOf(t, got, "b0000002"), "Supersedes a0000001.") {
		t.Fatalf("entries not linked:\n%s", got)
	}
	assertSubjectNames(t, "a0000001", "b0000002")
	if out, err := ReviseEntry(root, "a0000001", "b0000002", false); err != nil || !strings.Contains(out, "nothing to do") {
		t.Errorf("repeat = %q, %v; want nothing to do", out, err)
	}
}

// assertSubjectNames fails unless the store's last commit subject names every id.
func assertSubjectNames(t *testing.T, ids ...string) {
	t.Helper()
	subject := hosttest.In(t, StoreDir(), "log", "-1", "--format=%s")
	for _, id := range ids {
		if !strings.Contains(subject, id) {
			t.Errorf("commit subject %q must name %s", subject, id)
		}
	}
}

// TestReviseEntryRefusesBadIds: an unknown id, the same id twice and a cycle are input errors naming the id.
func TestReviseEntryRefusesBadIds(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	write(t, Dir(root)+"/decisionLog.md", reviseLog)
	for _, c := range [][2]string{{"a0000001", "a0000001"}, {"ffffffff", "b0000002"}, {"d0000004", "e0000005"}} {
		if _, err := ReviseEntry(root, c[0], c[1], false); !errors.Is(err, ErrReviseInput) {
			t.Errorf("revise %v err = %v; want ErrReviseInput", c, err)
		}
	}
}
