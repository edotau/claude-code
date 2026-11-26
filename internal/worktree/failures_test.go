package worktree

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/paths"
)

func TestFailures(t *testing.T) {
	root := t.TempDir()
	f := Failure{Kind: FailBench, Subject: "x", Summary: "boom\nline two", Time: time.Now().Unix()}
	if ok, err := ReportFailure(root, f); !ok || err != nil {
		t.Fatalf("first report = %v, %v", ok, err)
	}
	for _, s := range []string{"a", "b"} {
		got, more, err := FailuresSinceWithin(root, s, 3, time.Minute)
		if err != nil || len(got) != 1 || more != 0 || strings.Contains(got[0].Summary, "\n") {
			t.Fatalf("session %s first drain = %+v, %d, %v", s, got, more, err)
		}
		if got, _, _ := FailuresSinceWithin(root, s, 3, time.Minute); len(got) != 0 {
			t.Fatalf("session %s saw %+v twice", s, got)
		}
	}
	if ok, _ := ReportFailure(root, f); ok {
		t.Fatal("repeat inside the window was appended")
	}
	if ok, _ := ReportFailure(root, Failure{Kind: FailBench, Subject: "y"}); !ok {
		t.Fatal("distinct subject dropped")
	}
	if got, _, _ := FailuresSinceWithin(root, "a", 3, time.Minute); len(got) != 1 || got[0].Subject != "y" {
		t.Fatalf("known cursor drain = %+v, want only y", got)
	}
	if _, err := ReportFailure(root, Failure{Kind: "nope", Subject: "x"}); err == nil {
		t.Fatal("unknown kind accepted")
	}
	id := FailureID(FailBench, "x")
	if h, err := ClaimFailure(root, id, "a"); h != "a" || err != nil {
		t.Fatalf("first claim = %q, %v", h, err)
	}
	if h, _ := ClaimFailure(root, id, "b"); h != "a" {
		t.Fatalf("second claim holder = %q, want a", h)
	}
	if err := DoneFailure(root, id); err != nil {
		t.Fatal(err)
	}
	if h, done := FailureHolder(root, id); h != "a" || !done {
		t.Fatalf("holder = %q done = %v", h, done)
	}
	if _, err := ClaimFailure(root, "../../etc", "a"); err == nil {
		t.Fatal("path-like id accepted")
	}
}

func TestFailuresTrimKeepsNewest(t *testing.T) {
	root := t.TempDir()
	seedLog(t, root, 1200, "1189")
	if _, err := ReportFailure(root, Failure{Kind: FailAudit, Subject: "new"}); err != nil {
		t.Fatal(err)
	}
	recs, _ := ListFailures(root)
	if len(recs) != failureKeep || recs[len(recs)-1].Subject != "new" || recs[len(recs)-1].Seq != 1201 {
		t.Fatalf("after trim: %d records, last %+v", len(recs), recs[len(recs)-1])
	}
	if got, more, _ := FailuresSinceWithin(root, "s", 100, time.Minute); len(got) != 12 || more != 0 || got[0].Seq != 1190 {
		t.Fatalf("cursor at 1189 after trim = %d records, %d more, first %+v", len(got), more, got[0])
	}
}

func TestAutofixHeld(t *testing.T) {
	repo, root := t.TempDir(), t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "x"},
		{"branch", "worktree-autofix-abcdef0"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Skipf("git %v: %v %s", args, err, out)
		}
	}
	held, free := "lane@abcdef0", "lane@1234567"
	for _, subj := range []string{held, free} {
		if _, err := ReportFailure(root, Failure{Kind: FailAudit, Subject: subj, Repo: repo}); err != nil {
			t.Fatal(err)
		}
	}
	if h, _ := ClaimFailure(root, FailureID(FailAudit, held), "a"); h != "autofix" {
		t.Fatalf("held holder = %q, want autofix", h)
	}
	if h, _ := ClaimFailure(root, FailureID(FailAudit, free), "a"); h != "a" {
		t.Fatalf("free holder = %q, want a", h)
	}
}

// seedLog writes n padded records with Seq 1..n, the seq counter and a cursor for session "s".
func seedLog(t *testing.T, root string, n int, cursor string) {
	t.Helper()
	big := strings.Repeat("x", 1000)
	var b strings.Builder
	for i := range n {
		b.WriteString(`{"id":"` + FailureID(FailBench, strconv.Itoa(i)) + `","kind":"bench","subject":"` + strconv.Itoa(i) +
			`","summary":"` + big + `","time":1,"seq":` + strconv.Itoa(i+1) + `}` + "\n")
	}
	for name, body := range map[string]string{"failures.jsonl": b.String(), "seq": strconv.Itoa(n), "cursors/s-s": cursor} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFailureReopenedInsideWindow(t *testing.T) {
	root := t.TempDir()
	f := Failure{Kind: FailBench, Subject: "x"}
	if ok, _ := ReportFailure(root, f); !ok {
		t.Fatal("first report dropped")
	}
	id := FailureID(FailBench, "x")
	if _, err := ClaimFailure(root, id, "a"); err != nil {
		t.Fatal(err)
	}
	if err := DoneFailure(root, id); err != nil {
		t.Fatal(err)
	}
	if ok, _ := ReportFailure(root, f); !ok {
		t.Fatal("a fixed failure re-reported inside the window stayed hidden")
	}
	if h, done := FailureHolder(root, id); h != "" || done {
		t.Fatalf("after reopen holder = %q done = %v", h, done)
	}
}

func TestFailuresSinceWithinTimeoutKeepsCursor(t *testing.T) {
	root := t.TempDir()
	if _, err := ReportFailure(root, Failure{Kind: FailAudit, Subject: "a", Summary: "s"}); err != nil {
		t.Fatal(err)
	}
	err := paths.WithFileLock(failuresLog(root), func() error {
		_, _, err := FailuresSinceWithin(root, "sess", 5, 50*time.Millisecond)
		return err
	})
	if !errors.Is(err, paths.ErrLockTimeout) {
		t.Fatalf("err = %v, want ErrLockTimeout", err)
	}
	lines, _, err := FailuresSinceWithin(root, "sess", 5, time.Second)
	if err != nil || len(lines) != 1 {
		t.Fatalf("after timeout: lines=%d err=%v, want the failure still unseen", len(lines), err)
	}
}

func TestOverlayUnknownKeyFailureDedup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var keys []string
	for i := range 600 {
		keys = append(keys, "bogus"+strconv.Itoa(i))
	}
	for range 2 {
		agentconfig.ReportUnknownKey(keys, "typed")
	}
	got, err := ListFailures(FailuresDir())
	if err != nil || len(got) != 1 || got[0].Kind != FailOverlayKey || !strings.Contains(got[0].Subject, "+590 more") {
		t.Fatalf("failures = %+v, %v; want one overlay_unknown_key entry for 600 keys", got, err)
	}
}

// TestNextOffer: one session per lease, DirectiveMax deliveries, then the fallback once.
func TestNextOffer(t *testing.T) {
	lease := int64(offerLease / time.Second)
	o, res := nextOffer(failureOffer{}, "s1", 100, true)
	steps := []struct {
		slug     string
		at       int64
		fallback bool
		want     OfferResult
	}{
		{"s2", 100 + lease - 1, true, OfferNone},  // s1's lease
		{"s1", 100 + lease, true, OfferDirective}, // a delivery the hook may have lost comes back after the lease
		{"s2", 100 + lease, true, OfferNone},      // s1's second lease
		{"s3", 100 + 3*lease, false, OfferNone},   // Stop has no plain notice
		{"s3", 100 + 3*lease, true, OfferNotice},
		{"s4", 100 + 5*lease, true, OfferNone},
	}
	if res != OfferDirective {
		t.Fatalf("first offer = %v", res)
	}
	for i, s := range steps {
		if o, res = nextOffer(o, s.slug, s.at, s.fallback); res != s.want {
			t.Fatalf("step %d (%s) = %v, want %v (%+v)", i, s.slug, res, s.want, o)
		}
	}
	root := t.TempDir()
	id := FailureID(FailAudit, "c")
	if _, err := ClaimFailure(root, id, "peer"); err != nil {
		t.Fatal(err)
	}
	if res, err := OfferFailure(t.Context(), root, Failure{ID: id, Kind: FailAudit, Subject: "c"}, "s1", true); res != OfferOwned || err != nil {
		t.Fatalf("claimed offer = %v, %v", res, err)
	}
}

func TestNotePromptPrunesOnlyStaleStamps(t *testing.T) {
	root := t.TempDir()
	if err := NotePrompt(root, "old"); err != nil {
		t.Fatal(err)
	}
	if err := NotePrompt(root, "fresh"); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-promptKeep - time.Hour)
	if err := os.Chtimes(promptStamp(root, "old"), past, past); err != nil {
		t.Fatal(err)
	}
	if err := NotePrompt(root, "third"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(promptStamp(root, "old")); !os.IsNotExist(err) {
		t.Fatalf("week-old stamp: %v, want pruned", err)
	}
	if _, err := os.Stat(promptStamp(root, "fresh")); err != nil {
		t.Fatalf("fresh stamp: %v, want kept", err)
	}
}

func TestFailureRefInGitDir(t *testing.T) {
	g := t.TempDir()
	ref := "refs/heads/worktree-autofix-abc1234"
	if found, ok := refInGitDir(g, ref); found || !ok {
		t.Fatalf("empty = %v,%v", found, ok)
	}
	if err := os.WriteFile(filepath.Join(g, "packed-refs"), []byte("# pack\n1234 "+ref+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if found, ok := refInGitDir(g, ref); !found || !ok {
		t.Fatalf("packed = %v,%v", found, ok)
	}
	if found, _ := refInGitDir(g, ref+"x"); found {
		t.Fatal("prefix matched")
	}
	_ = os.Remove(filepath.Join(g, "packed-refs"))
	_ = os.MkdirAll(filepath.Join(g, "refs", "heads"), 0o700)
	_ = os.WriteFile(filepath.Join(g, "refs", "heads", "worktree-autofix-abc1234"), []byte("1\n"), 0o600)
	if found, ok := refInGitDir(g, ref); !found || !ok {
		t.Fatalf("loose = %v,%v", found, ok)
	}
	if _, ok := refInGitDir(filepath.Join(g, "nope"), ref); ok {
		t.Fatal("missing dir must defer to git")
	}
}
