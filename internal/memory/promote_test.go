package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedConventionBlock plants one id-marked conventions.md bullet in root's local bank and returns its id.
func seedConventionBlock(t *testing.T, root string) string {
	t.Helper()
	id, err := Remember(root, "feedback", "promote me to home", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// judgmentFixture seeds a proj bank with one convention block; before is its conventions.md.
func judgmentFixture(t *testing.T) (root, id, localPath string, before []byte) {
	t.Helper()
	tempConfig(t)
	root = repoRoot(t, "proj")
	id = seedConventionBlock(t, root)
	localPath = BankFor(root).Path("conventions.md")
	before, _ = os.ReadFile(localPath)
	return root, id, localPath, before
}

// TestPromoteJudgmentTargetsPrintOnly: rules/skill only print the block and procedure.
func TestPromoteJudgmentTargetsPrintOnly(t *testing.T) {
	ref, err := Reference("promote")
	if err != nil {
		t.Fatalf("Reference(\"promote\"): %v", err)
	}
	root, id, localPath, before := judgmentFixture(t)
	homePath := filepath.Join(StoreDir(), "conventions.md")
	_, homeErrBefore := os.Stat(homePath)
	for _, to := range []string{"rules", "skill"} {
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
	}
}

// TestPromoteClaudeMDNeedsADreamRun: claude-md is refused outside a dream run and printed inside one.
func TestPromoteClaudeMDNeedsADreamRun(t *testing.T) {
	root, id, localPath, before := judgmentFixture(t)
	t.Run("RefusedOutsideDream", func(t *testing.T) {
		t.Setenv(dreamChildEnv, "")
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
	})
	t.Run("AllowedInsideDream", func(t *testing.T) {
		t.Setenv(dreamChildEnv, "1")
		out, err := Promote(root, id, "claude-md", false, time.Now())
		if err != nil {
			t.Fatalf("Promote --to claude-md inside a dream run: %v", err)
		}
		if !strings.Contains(out, "target: ") {
			t.Errorf("--to claude-md output missing the target line: %q", out)
		}
	})
}

// TestPromoteUnknownTargetErrors: an unmapped --to is refused, naming the valid set, before any bank access.
func TestPromoteUnknownTargetErrors(t *testing.T) {
	if _, err := Promote("", "x", "bogus", false, time.Now()); err == nil || !strings.Contains(err.Error(), "want") {
		t.Fatalf("Promote --to bogus = %v, want an error naming the valid targets", err)
	}
}

// TestPromoteUnknownIDErrors: a --to home on a nonexistent id is refused rather than silently no-op.
func TestPromoteUnknownIDErrors(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj") // scaffolds the bank; the id below is deliberately wrong
	if _, err := Promote(root, "deadbeef", "rules", false, time.Now()); err == nil {
		t.Fatal("Promote with an unknown id = nil error, want one")
	}
}
