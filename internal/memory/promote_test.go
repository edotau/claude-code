package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

func TestPrunePreservesEntryBytes(t *testing.T) {
	for _, name := range []string{"conventions.md", "decisionLog.md", "progress.md"} {
		t.Run(name, func(t *testing.T) {
			tempConfig(t)
			head := "# Bank\n\n\n"
			old, kept := "- old <!-- id:12345678 -->\n\n\n", "- newest <!-- id:23456789 -->"
			if name == "decisionLog.md" {
				old, kept = "## 2026-01-01 — old <!-- id:12345678 -->\n\n\n", "## 2026-11-26 — newest <!-- id:23456789 -->"
			}
			body := head + old + kept
			want := head + kept
			if name == "progress.md" {
				head += "## What Works\n\n\n"
				kept += "\n\n\n"
				body, want = head+kept+old, head+kept[:len(kept)-1] // the newline joining the moved entry goes with it
			}
			write(t, Path(name), body)
			if n, err := archiveFile(name, 1, testEnv().Now); err != nil || n != 1 {
				t.Fatalf("archive = %d, %v", n, err)
			}
			if got := mustRead(t, Path(name)); got != want {
				t.Errorf("kept bytes changed: got %q want %q", got, want)
			}
			if got := mustRead(t, archivePath(name, testEnv().Now)); !strings.Contains(got, old) {
				t.Errorf("moved bytes changed: %q", got)
			}
		})
	}
}

func TestPruneKeepsWhitespaceDistinctEntries(t *testing.T) {
	d := ParseDoc("conventions.md", []byte("# C\n\n- same words\n\n- same  words\n"))
	got, moved := partitionDoc(d, []string{"- same  words"})
	if got != "# C\n\n- same  words\n" || len(moved) != 1 || moved[0] != "- same words\n\n" {
		t.Fatalf("wrong entry selected: kept %q moved %q", got, moved)
	}
	d = ParseDoc("conventions.md", []byte("# C\n\n- same\n\n\n- same\n")) // equal keys: the newer, lower one stays
	_, keep, _ := docOverflow(d, 1)
	if got, moved := partitionDoc(d, keep); got != "# C\n\n- same\n" || len(moved) != 1 || moved[0] != "- same\n\n\n" {
		t.Fatalf("older duplicate kept: kept %q moved %q", got, moved)
	}
}

// A rotation or prune leaves exactly what the store renders once the moved rows leave the doc (section headings stay).
func TestPartitionDocMatchesStoreRender(t *testing.T) {
	var hist strings.Builder
	hist.WriteString("# Session History\n\n> Newest first.\n\n")
	for i := sessionHistoryMaxEntries; i >= 0; i-- {
		fmt.Fprintf(&hist, "## 2026-11-%02d — s%d <!-- id:%08x -->\n\nbody\n\n", i%28+1, i, i+1)
	}
	cases := map[string]string{
		"sessionHistory.md": strings.TrimSuffix(hist.String(), "\n"),
		"conventions.md":    "# C\n\n- old <!-- id:12345678 -->\n- newest <!-- id:23456789 -->\n",
		"progress.md":       "# P\n\n## What Works\n\n- newest <!-- id:23456789 -->\n- old <!-- id:12345678 -->\n",
	}
	for name, body := range cases {
		d := ParseDoc(name, []byte(body))
		_, keep, _ := docOverflow(d, 1)
		if name == "sessionHistory.md" {
			_, keep, _ = sessionOverflow(body)
		}
		text, moved := partitionDoc(d, keep)
		rows := d
		rows.Entries = slices.DeleteFunc(slices.Clone(d.Entries), func(e DocEntry) bool {
			return e.ID != "" && slices.ContainsFunc(moved, func(m string) bool { return strings.Contains(m, strings.TrimRight(e.block(), "\n")) })
		})
		if len(moved) == 0 || len(rows.Entries) == 0 || text != string(rows.Render()) || !rowsRoundTrip(ParseDoc(name, []byte(text))) {
			t.Errorf("%s: retained %q, store renders %q", name, text, rows.Render())
		}
	}
}

func TestPruneKeepsOutsideArchiveEdit(t *testing.T) {
	tempConfig(t)
	path := Path("conventions.md")
	body := "# C\n\n- old <!-- id:12345678 -->\n\n- newest <!-- id:23456789 -->\n"
	write(t, path, body)
	archive := archivePath("conventions.md", testEnv().Now)
	write(t, archive, "# Archive\n")
	outside := "# Archive\n\n- outside\n"
	prev := readFile
	t.Cleanup(func() { readFile = prev })
	readFile = func(p string) ([]byte, error) {
		b, err := os.ReadFile(p)
		if p == archive && err == nil {
			outside += "more\n"
			write(t, archive, outside)
		}
		return b, err
	}
	if _, err := archiveFile("conventions.md", 1, testEnv().Now); err == nil {
		t.Fatal("archive conflict reported as success")
	}
	if got := mustRead(t, archive); got != outside {
		t.Errorf("outside archive edit lost: %q", got)
	}
	if got := mustRead(t, path); got != body {
		t.Errorf("source pruned after archive conflict: %q", got)
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
	all := rejoin(header, keep) + strings.Join(overflow, "\n")
	if got := strings.Count(all, "- Focus bullet "); got != 16 {
		t.Fatalf("merged section lost bullets: got %d of 16: %s", got, all)
	}
	if strings.Contains(strings.Join(overflow, "\n"), "## Focus\n") {
		t.Fatalf("repeat was archived as a separate section: %q", overflow)
	}
}

func TestArchiveNormalisedBlockOnce(t *testing.T) {
	tempConfig(t)
	now := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	path := Path("progress.md")
	body := "# Progress\n\n## In Progress\n\n- first\n\n## Known Issues\n\n- issue\n\n## In Progress\n\n- second\n"
	write(t, path, body)
	if n, err := archiveFile("progress.md", 7, now); err != nil {
		t.Fatal(err)
	} else if n == 0 {
		t.Fatal("expected overflow")
	}
	live := mustRead(t, path)
	if strings.Count(live, "## In Progress") != 1 {
		t.Fatalf("repeated heading not merged: %q", live)
	}
	archive := archivePath("progress.md", now)
	before := mustRead(t, archive)
	// Simulate a failed source rewrite: same content reappears with different whitespace.
	write(t, path, strings.ReplaceAll(body, "- second", "-  second"))
	if _, err := archiveFile("progress.md", 7, now); err != nil {
		t.Fatal(err)
	}
	if after := mustRead(t, archive); after != before {
		t.Fatalf("normalised archived block appended twice:\nbefore: %q\nafter: %q", before, after)
	}
}
