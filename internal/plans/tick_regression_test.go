package plans

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

func TestUnreadableMirrorRecordPreventsTick(t *testing.T) {
	for _, scope := range []string{"repo", "top-level"} {
		t.Run(scope, func(t *testing.T) {
			config := hermetic(t)
			repo, base := tickRangeRepo(t)
			home := Home(repo)
			if scope == "top-level" {
				home = filepath.Join(config, "plans")
			}
			plan := filepath.Join(home, "2026-11-09-draft.md")
			body := "- [ ] 1.1 Draft owns: internal/a/a.go\n"
			writeFile(t, plan, body)
			if err := os.MkdirAll(DestRecordPath("draft"), 0o700); err != nil {
				t.Fatal(err)
			}
			if _, err := os.ReadFile(DestRecordPath("draft")); err == nil {
				t.Fatal("fixture record must be unreadable")
			}
			drafts := MirrorDests()
			if !slices.Contains(drafts, plan) {
				t.Fatalf("unreadable record left readable draft out of exclusions: %v", drafts)
			}
			if err := TickRange(context.Background(), repo, "", base+"..HEAD", "", drafts, false, io.Discard); err != nil {
				t.Fatal(err)
			}
			wantBytes(t, plan, "unapproved draft", body)
		})
	}
}

func TestClearClaimOwnedRejectsEmptyOwner(t *testing.T) {
	plan := userTickPlan(t, "- [ ] 1.1 Task owns: a.go <!-- working: alice -->\n")
	before := readFile(t, plan)
	if err := ClearClaimOwned(plan, "1.1", ""); err != nil {
		t.Fatal(err)
	}
	wantBytes(t, plan, "alice's claim with empty caller", before)
}

func TestTickRangePropagatesOutputErrors(t *testing.T) {
	cases := []struct {
		name, owns, explicit, output string
		plans                        int
	}{
		{"no owner", "other.go", "", "no plan owns", 1},
		{"tie updates both", "internal/a/a.go", "", "candidate", 2},
		{"nothing ticked", "other.go", "2026-11-09-0.md", "nothing to tick in", 1},
		{"ticked", "internal/a/a.go", "2026-11-09-0.md", "candidate", 1},
		{"partial", "internal/a/a.go, missing.go", "2026-11-09-0.md", "candidate", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hermetic(t)
			repo, base := tickRangeRepo(t)
			home := Home(repo)
			for i := range tc.plans {
				name := "2026-11-09-" + strconv.Itoa(i) + ".md"
				writeFile(t, filepath.Join(home, name), "- [ ] 1.1 Task owns: "+tc.owns+"\n")
			}
			plan := ""
			if tc.explicit != "" {
				plan = filepath.Join(home, tc.explicit)
			}
			rng := base + ".." + strings.TrimSpace(hosttest.In(t, repo, "rev-parse", "HEAD~2"))
			var output bytes.Buffer
			if err := TickRange(context.Background(), repo, "", rng, plan, nil, true, &output); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output.String(), tc.output) {
				t.Fatalf("wrong output branch: %q, want %q", output.String(), tc.output)
			}
			reader, writer := io.Pipe()
			defer writer.Close()
			if err := reader.Close(); err != nil {
				t.Fatal(err)
			}
			if err := TickRange(context.Background(), repo, "", rng, plan, nil, true, writer); !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("TickRange = %v, want closed pipe error", err)
			}
		})
	}
}

// An emptied record names no mirror; a truncated or otherwise unreadable one excludes every plan (fail closed).
func TestMirrorDestsTreatsTruncatedRecordAsAbsent(t *testing.T) {
	hermetic(t)
	live := filepath.Join(Home(t.TempDir()), "2026-11-09-live.md")
	approved := filepath.Join(filepath.Dir(live), "2026-11-09-approved.md") // filed from a draft whose record was retired
	other := filepath.Join(ScratchDir(), "other.dest")
	writeFile(t, live, "- [ ] 1.1 Live\n")
	writeFile(t, approved, "- [ ] 1.1 Approved\n")
	writeFile(t, other, "")
	if err := RecordDest("live", live); err != nil {
		t.Fatal(err)
	}
	bad := map[string]string{"emptied": "", "truncated": live[:len(live)-6], "relative": "plans/x.md", "two lines": live + "\nx.md"}
	for name, body := range bad {
		writeFile(t, other, body)
		if got, ok := ReadRecordedDest("other"); ok {
			t.Errorf("%s record %q read back as destination %q", name, body, got)
		}
		if got := MirrorDests(); slices.Contains(got, approved) != (name != "emptied") || !slices.Contains(got, live) {
			t.Errorf("%s record: MirrorDests = %v (only the emptied one leaves approved out)", name, got)
		}
	}
	if err := os.Remove(other); err != nil || os.MkdirAll(DestRecordPath("other"), 0o700) != nil {
		t.Fatal("cannot build an unreadable record")
	}
	if got := MirrorDests(); !slices.Contains(got, approved) || !slices.Contains(got, live) {
		t.Errorf("unreadable record must still fail closed to every plan: %v", got)
	}
}

// Only a real trailer names a task: quoted, indented, prose-adjacent and first-paragraph lines never do.
func TestTaskTrailerMustBeAGitTrailer(t *testing.T) {
	const line = "Task: 2026-11-18-p 1.1"
	cases := []struct {
		name, msg string
		want      bool
	}{
		{"trailer", "feat: a\n\n" + line, true},
		{"trailer with sign-off", "feat: a\n\nBody.\n\nSigned-off-by: A <a@b>\n" + line + "\n", true},
		{"crlf", "feat: a\r\n\r\n" + line + "\r\n", true},
		{"indented quote", "revert: q\n\nOriginal message:\n    " + line + "\n\nNot completed.", false},
		{"indented last paragraph", "revert: q\n\n    " + line, false},
		{"angle quote", "revert: q\n\n> " + line, false},
		{"prose after", "fix: p\n\n" + line + "\nThis was only an example.", false},
		{"not the last paragraph", "fix: p\n\n" + line + "\n\nMore body.", false},
		{"subject paragraph", "fix: p\n" + line, false},
		{"squash summary", "Squashed commit of the following:\n\ncommit abc\nAuthor: A <a@b>\n\n    feat: x\n\n    " + line + "\n", false},
	}
	for _, c := range cases {
		if got := namesTask(c.msg, "/p/2026-11-18-p.md", "1.1"); got != c.want {
			t.Errorf("%s: namesTask = %v, want %v", c.name, got, c.want)
		}
	}
}

// A merge commit's own trailer ticks when the range lands, which --no-merges used to hide.
func TestTickRangeCountsMergeCommitTrailer(t *testing.T) {
	hermetic(t)
	repo := gitRepo(t, "main")
	plan := filepath.Join(Home(repo), "2026-11-18-p.md")
	writeFile(t, plan, "- [ ] 1.1 One\n")
	hosttest.In(t, repo, "checkout", "-q", "-b", "side")
	hosttest.In(t, repo, "commit", "-q", "--allow-empty", "-m", "feat: side")
	hosttest.In(t, repo, "checkout", "-q", "main")
	hosttest.In(t, repo, "commit", "-q", "--allow-empty", "-m", "feat: main")
	hosttest.In(t, repo, "merge", "-q", "--no-ff", "side", "-m", "Merge side\n\nTask: 2026-11-18-p 1.1")
	if err := TickRange(context.Background(), repo, "", "HEAD^1..HEAD", "", nil, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, plan); !strings.Contains(got, "[x] 1.1") {
		t.Fatalf("the merge commit's trailer must tick:\n%s", got)
	}
}

// With no trunk to name, a named commit ticks nothing and says so; a repo with no plans stays silent.
func TestTickCommitWithoutTrunkTicksNothing(t *testing.T) {
	hermetic(t)
	repo := gitRepo(t, "trunk")
	if _, _, err := TickCommit(repo, "HEAD", nil); err != nil {
		t.Fatalf("no plans, no complaint: %v", err)
	}
	plan := filepath.Join(Home(repo), "2026-11-18-p.md")
	writeFile(t, plan, "- [ ] 1.1 One\n")
	hosttest.In(t, repo, "commit", "-q", "--allow-empty", "-m", "feat: a\n\nTask: 2026-11-18-p 1.1")
	if _, _, err := TickCommit(repo, "HEAD", nil); !errors.Is(err, ErrNoTrunk) {
		t.Fatalf("err = %v, want ErrNoTrunk", err)
	}
	wantBytes(t, plan, "no trunk", "- [ ] 1.1 One\n")
}

// --plan must name a dated plan in the repo's plans home, the only place landing looks the trailer's slug up.
func TestTaskTrailersRefuseUndiscoverablePlan(t *testing.T) {
	hermetic(t)
	repo := gitRepo(t, "main")
	outside := filepath.Join(t.TempDir(), "2026-11-18-outside.md")
	undated := filepath.Join(Home(repo), "notes.md")
	writeFile(t, outside, "- [ ] 1.1 One\n")
	writeFile(t, undated, "- [ ] 1.1 One\n")
	for _, plan := range []string{outside, undated} {
		if got, err := TaskTrailers(repo, plan, []string{"1.1"}); err == nil || !strings.Contains(err.Error(), "not a dated plan in") {
			t.Errorf("%s: got %q, %v, want a refusal", plan, got, err)
		}
	}
}

// A reopened task is announced again, a ticked one forgotten, and the memo never grows past memoCap.
func TestCandidateMemoResetsAndIsCapped(t *testing.T) {
	hermetic(t)
	plan := filepath.Join(t.TempDir(), "2026-11-18-m.md")
	writeFile(t, plan, "- [ ] 1.1 One\n")
	if !announce(plan, "1.1", false) || announce(plan, "1.1", false) {
		t.Fatal("a candidate is announced once")
	}
	if err := MarkDone(plan, "1.1", ""); err != nil {
		t.Fatal(err)
	}
	if !announce(plan, "1.1", false) {
		t.Fatal("a ticked task must be forgotten")
	}
	if err := Reopen(plan, "1.1"); err != nil {
		t.Fatal(err)
	}
	if !announce(plan, "1.1", false) {
		t.Fatal("a reopened task must be announced again")
	}
	for i := range memoCap + 10 {
		announce(plan, strconv.Itoa(i), false)
	}
	if n := len(readMemo()); n != memoCap {
		t.Fatalf("memo has %d keys, want the cap %d", n, memoCap)
	}
}
