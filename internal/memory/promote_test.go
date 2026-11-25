package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/paths"
)

// seedConventionBlock plants one id-marked conventions.md bullet in the bank and returns its id.
func seedConventionBlock(t *testing.T) string {
	t.Helper()
	id, err := Remember("feedback", "promote me to home", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// judgmentFixture seeds a proj bank with one convention block; before is its conventions.md.
func judgmentFixture(t *testing.T) (id, localPath string, before []byte) {
	t.Helper()
	tempConfig(t)
	repoRoot(t, "proj")
	id = seedConventionBlock(t)
	localPath = Path("conventions.md")
	before, _ = os.ReadFile(localPath)
	return id, localPath, before
}

// TestPromoteJudgmentTargetsPrintOnly: rules/skill only print the block and procedure.
func TestPromoteJudgmentTargetsPrintOnly(t *testing.T) {
	ref := assets.MemoryPromoteProcedure()
	id, localPath, before := judgmentFixture(t)
	homePath := filepath.Join(paths.MemoryStoreDir(), "conventions.md")
	_, homeErrBefore := os.Stat(homePath)
	for _, to := range []string{"rules", "skill"} {
		out, err := Promote(id, to)
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
	id, localPath, before := judgmentFixture(t)
	t.Run("RefusedOutsideDream", func(t *testing.T) {
		t.Setenv(dreamChildEnv, "")
		_, err := Promote(id, "claude-md")
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
		out, err := Promote(id, "claude-md")
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
	if _, err := Promote("x", "bogus"); err == nil || !strings.Contains(err.Error(), "want") {
		t.Fatalf("Promote --to bogus = %v, want an error naming the valid targets", err)
	}
}

// TestPromoteUnknownIDErrors: a --to rules on a nonexistent id is refused rather than silently no-op.
func TestPromoteUnknownIDErrors(t *testing.T) {
	tempConfig(t)
	repoRoot(t, "proj") // scaffolds the bank; the id below is deliberately wrong
	if _, err := Promote("deadbeef", "rules"); err == nil {
		t.Fatal("Promote with an unknown id = nil error, want one")
	}
}

func TestSectionSplitKeepsEverySection(t *testing.T) {
	var b strings.Builder
	b.WriteString("# Active\n\n")
	for _, h := range []string{"Focus", "Next", "Blockers", "Focus"} {
		b.WriteString("## " + h + "\n\n")
		for i := range 8 {
			fmt.Fprintf(&b, "- %s bullet %d\n", h, i)
		}
		b.WriteString("\n")
	}
	header, keep, overflow := overflowSplit("activeContext.md", b.String(), 20)
	if got := logicalLineCount(rejoin(header, keep)); got > 20 {
		t.Fatalf("kept %d lines, want <= 20", got)
	}
	heads := strings.Join(keep, "\n")
	for _, h := range []string{"## Focus", "## Next", "## Blockers"} {
		if strings.Count(heads, h+"\n") != 1 {
			t.Fatalf("heading %q not kept exactly once:\n%s", h, heads)
		}
	}
	if !strings.Contains(strings.Join(overflow, "\n"), "- Focus bullet 7") || len(overflow) < 4 {
		t.Fatalf("overflow missing the duplicate section or trimmed bullets: %q", overflow)
	}
}
