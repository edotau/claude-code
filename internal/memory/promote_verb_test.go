package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedConventionBlock plants one id-marked conventions.md bullet in root's local bank via Remember,
// returning its id — the subject every Promote test moves/surfaces.
func seedConventionBlock(t *testing.T, root string) string {
	t.Helper()
	id, err := Remember(root, "feedback", "promote me to home", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestPromoteHomeMovesExactlyOneBlock: --to home removes the block from the local bank and adds it,
// unchanged (same id), to the matching home-bank file — nothing else in either file changes.
func TestPromoteHomeMovesExactlyOneBlock(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	id := seedConventionBlock(t, root)
	localBefore, _ := os.ReadFile(filepath.Join(Dir(root), "conventions.md"))

	out, err := Promote(root, id, "home", false, time.Now())
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if !strings.Contains(out, id) || !strings.Contains(out, "home") {
		t.Errorf("Promote output missing id/target: %q", out)
	}
	localAfter, _ := os.ReadFile(filepath.Join(Dir(root), "conventions.md"))
	if strings.Contains(string(localAfter), "<!-- id:"+id+" -->") {
		t.Errorf("local bank still has the promoted block: %q", localAfter)
	}
	if string(localBefore) == string(localAfter) {
		t.Error("local bank was not rewritten at all")
	}
	homeData, err := os.ReadFile(filepath.Join(HomeBankDir(), "conventions.md"))
	if err != nil || !strings.Contains(string(homeData), "<!-- id:"+id+" -->") {
		t.Fatalf("home bank missing the promoted block (id %s): %v %q", id, err, homeData)
	}
	// Exactly one block moved: the id appears exactly once across the home file.
	if n := strings.Count(string(homeData), "<!-- id:"+id+" -->"); n != 1 {
		t.Errorf("home bank has the block %d times, want 1", n)
	}
}

// TestPromoteHomeDryRunWritesNothing: --dry-run prints the move without touching either file.
func TestPromoteHomeDryRunWritesNothing(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	id := seedConventionBlock(t, root)
	localPath := filepath.Join(Dir(root), "conventions.md")
	homePath := filepath.Join(HomeBankDir(), "conventions.md")
	localBefore, _ := os.ReadFile(localPath)
	_, homeErrBefore := os.Stat(homePath)

	out, err := Promote(root, id, "home", true, time.Now())
	if err != nil {
		t.Fatalf("Promote --dry-run: %v", err)
	}
	if !strings.Contains(out, "would move") || !strings.Contains(out, id) {
		t.Errorf("dry-run output missing the preview: %q", out)
	}
	localAfter, err := os.ReadFile(localPath)
	if err != nil || string(localBefore) != string(localAfter) {
		t.Errorf("--dry-run changed the local file: before=%q after=%q err=%v", localBefore, localAfter, err)
	}
	if _, err := os.Stat(homePath); (homeErrBefore == nil) != (err == nil) {
		t.Errorf("--dry-run changed whether the home file exists: before-err=%v after-err=%v", homeErrBefore, err)
	} else if homeErrBefore != nil && err == nil {
		t.Error("--dry-run created the home bank file")
	}
}

// TestPromoteJudgmentTargetsWriteNothingAndPrintTheProcedure: rules/skill never move or copy the
// block anywhere — Go prints the block, the target, and the embedded promote procedure inline.
func TestPromoteJudgmentTargetsWriteNothingAndPrintTheProcedure(t *testing.T) {
	ref, err := Reference("promote")
	if err != nil {
		t.Fatalf("Reference(\"promote\"): %v", err)
	}
	for _, to := range []string{"rules", "skill"} {
		t.Run(to, func(t *testing.T) {
			tempConfig(t)
			root := repoRoot(t, "proj")
			id := seedConventionBlock(t, root)
			localPath := filepath.Join(Dir(root), "conventions.md")
			before, _ := os.ReadFile(localPath)
			homePath := filepath.Join(HomeBankDir(), "conventions.md")
			_, homeErrBefore := os.Stat(homePath)

			out, err := Promote(root, id, to, false, time.Now())
			if err != nil {
				t.Fatalf("Promote --to %s: %v", to, err)
			}
			if !strings.Contains(out, "target: ") {
				t.Errorf("--to %s output missing the target line: %q", to, out)
			}
			if !strings.Contains(out, ref) {
				t.Errorf("--to %s output missing the embedded promote procedure:\n%s", to, out)
			}
			after, err := os.ReadFile(localPath)
			if err != nil || string(before) != string(after) {
				t.Errorf("--to %s rewrote the local bank: before=%q after=%q err=%v", to, before, after, err)
			}
			if _, err := os.Stat(homePath); homeErrBefore != nil && err == nil {
				t.Errorf("--to %s wrote the home bank file that didn't exist before", to)
			}
		})
	}
}

// TestPromoteClaudeMDRefusedOutsideDream: --to claude-md outside a dream run is refused with the
// reason, naming the --to rules alternative, and writes nothing.
func TestPromoteClaudeMDRefusedOutsideDream(t *testing.T) {
	t.Setenv(DreamChildEnv, "")
	tempConfig(t)
	root := repoRoot(t, "proj")
	id := seedConventionBlock(t, root)
	localPath := filepath.Join(Dir(root), "conventions.md")
	before, _ := os.ReadFile(localPath)

	_, err := Promote(root, id, "claude-md", false, time.Now())
	if err == nil {
		t.Fatal("Promote --to claude-md outside a dream run = nil error, want one")
	}
	if !strings.Contains(err.Error(), "memory dream") || !strings.Contains(err.Error(), "--to rules") {
		t.Errorf("error missing the reason/alternative: %v", err)
	}
	after, rerr := os.ReadFile(localPath)
	if rerr != nil || string(before) != string(after) {
		t.Errorf("--to claude-md rewrote the local bank: before=%q after=%q err=%v", before, after, rerr)
	}
}

// TestPromoteClaudeMDAllowedInsideDream: inside a dream run (DreamChildEnv set) --to claude-md
// prints the block, target, and procedure instead of refusing.
func TestPromoteClaudeMDAllowedInsideDream(t *testing.T) {
	t.Setenv(DreamChildEnv, "1")
	tempConfig(t)
	root := repoRoot(t, "proj")
	id := seedConventionBlock(t, root)

	out, err := Promote(root, id, "claude-md", false, time.Now())
	if err != nil {
		t.Fatalf("Promote --to claude-md inside a dream run: %v", err)
	}
	if !strings.Contains(out, "target: ") {
		t.Errorf("--to claude-md output missing the target line: %q", out)
	}
}

// TestPromoteUnknownTargetErrors: an unmapped --to is refused, naming the valid set.
func TestPromoteUnknownTargetErrors(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	id := seedConventionBlock(t, root)
	if _, err := Promote(root, id, "bogus", false, time.Now()); err == nil || !strings.Contains(err.Error(), "want") {
		t.Fatalf("Promote --to bogus = %v, want an error naming the valid targets", err)
	}
}

// TestPromoteUnknownIDErrors: a --to home on a nonexistent id is refused rather than silently no-op.
func TestPromoteUnknownIDErrors(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	seedConventionBlock(t, root) // scaffold the bank; the id below is deliberately wrong
	if _, err := Promote(root, "deadbeef", "home", false, time.Now()); err == nil {
		t.Fatal("Promote with an unknown id = nil error, want one")
	}
}
