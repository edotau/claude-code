package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
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

// TestPromoteHomeMovesExactlyOneBlock: --dry-run first prints the move without touching either file; then
// --to home removes the block from the local bank and adds it, unchanged (same id), to the matching home-bank file.
func TestPromoteHomeMovesExactlyOneBlock(t *testing.T) {
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
	if localDry, err := os.ReadFile(localPath); err != nil || string(localBefore) != string(localDry) {
		t.Errorf("--dry-run changed the local file: before=%q after=%q err=%v", localBefore, localDry, err)
	}
	if _, err := os.Stat(homePath); (homeErrBefore == nil) != (err == nil) {
		t.Errorf("--dry-run changed whether the home file exists: before-err=%v after-err=%v", homeErrBefore, err)
	} else if homeErrBefore != nil && err == nil {
		t.Error("--dry-run created the home bank file")
	}

	out, err = Promote(root, id, "home", false, time.Now())
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if !strings.Contains(out, id) || !strings.Contains(out, "home") {
		t.Errorf("Promote output missing id/target: %q", out)
	}
	localAfter, _ := os.ReadFile(localPath)
	if strings.Contains(string(localAfter), "<!-- id:"+id+" -->") {
		t.Errorf("local bank still has the promoted block: %q", localAfter)
	}
	if string(localBefore) == string(localAfter) {
		t.Error("local bank was not rewritten at all")
	}
	homeData, err := os.ReadFile(homePath)
	if err != nil || !strings.Contains(string(homeData), "<!-- id:"+id+" -->") {
		t.Fatalf("home bank missing the promoted block (id %s): %v %q", id, err, homeData)
	}
	// Exactly one block moved: the id appears exactly once across the home file.
	if n := strings.Count(string(homeData), "<!-- id:"+id+" -->"); n != 1 {
		t.Errorf("home bank has the block %d times, want 1", n)
	}
}

// TestPromoteHomeMovesArchivedBlock: an archived block promotes; the archive keeps its other blocks.
func TestPromoteHomeMovesArchivedBlock(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	arch := filepath.Join(Dir(root), "archive", "conventions-2026-11.md")
	if err := os.MkdirAll(filepath.Dir(arch), 0o700); err != nil {
		t.Fatal(err)
	}
	body := "# Conventions\n\n- keep me <!-- id:aaaa0001 -->\n\n- promote me <!-- id:aaaa0002 -->\n"
	if err := os.WriteFile(arch, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Promote(root, "aaaa0002", "home", false, time.Now()); err != nil {
		t.Fatalf("Promote archived block: %v", err)
	}
	after, _ := os.ReadFile(arch)
	if strings.Contains(string(after), "id:aaaa0002") || !strings.Contains(string(after), "id:aaaa0001") {
		t.Errorf("archive should drop only the promoted block: %q", after)
	}
	home, _ := os.ReadFile(filepath.Join(HomeBankDir(), "conventions.md"))
	if strings.Count(string(home), "id:aaaa0002") != 1 {
		t.Errorf("home conventions.md should hold the block once: %q", home)
	}
}

// TestPromoteJudgmentTargets: rules/skill/claude-md never move or copy the block — Go prints the block, the target,
// and the embedded procedure inline; claude-md is refused outside a dream run.
func TestPromoteJudgmentTargets(t *testing.T) {
	ref, err := Reference("promote")
	if err != nil {
		t.Fatalf("Reference(\"promote\"): %v", err)
	}
	tempConfig(t)
	root := repoRoot(t, "proj")
	id := seedConventionBlock(t, root)
	localPath := filepath.Join(Dir(root), "conventions.md")
	before, _ := os.ReadFile(localPath)
	homePath := filepath.Join(HomeBankDir(), "conventions.md")
	_, homeErrBefore := os.Stat(homePath)

	t.Run("WriteNothingAndPrintTheProcedure", func(t *testing.T) {
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
	})

	t.Run("ClaudeMDRefusedOutsideDream", func(t *testing.T) {
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

	t.Run("ClaudeMDAllowedInsideDream", func(t *testing.T) {
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
	if _, err := Promote(root, "deadbeef", "home", false, time.Now()); err == nil {
		t.Fatal("Promote with an unknown id = nil error, want one")
	}
}

// TestPromoteHomeSettlesInsideItsMoveLock: no second writer can take the bank lock between the move and the settle.
func TestPromoteHomeSettlesInsideItsMoveLock(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	id := seedConventionBlock(t, root)
	origWait, origHook := BankLockWait, afterPromoteMove
	t.Cleanup(func() { BankLockWait, afterPromoteMove = origWait, origHook })
	BankLockWait = 200 * time.Millisecond
	release := make(chan struct{})
	intruded := false
	afterPromoteMove = func() { // a rival writer that grabs the lock if it is free and holds it past BankLockWait
		held, refused := make(chan struct{}), make(chan struct{})
		go func() {
			hold := func() error { close(held); <-release; return nil }
			if ran, _ := paths.TryFileLock(BankLockPath(), time.Millisecond, hold); !ran {
				close(refused)
			}
		}()
		select {
		case <-held:
			intruded = true
		case <-refused:
		}
	}
	_, err := Promote(root, id, "home", false, time.Now())
	close(release)
	if intruded || err != nil {
		t.Fatalf("Promote released the bank lock before settling: rival took it = %v, Promote err = %v", intruded, err)
	}
}
