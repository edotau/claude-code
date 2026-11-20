package memory

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"github.com/edotau/claude-code/internal/paths"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func migFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	dir := paths.MemoryStoreDir()
	for rel, body := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"},
		{"add", "-A"}, {"commit", "-qm", "pre"}} {
		if out, err := migGit(dir, args...); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	return dir
}

func migHash(t *testing.T, dir string) [32]byte {
	t.Helper()
	h := sha256.New()
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if d.IsDir() && d.Name() == ".git" {
			return fs.SkipDir
		}
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			h.Write(append([]byte(p), b...))
		}
		return err
	})
	return [32]byte(h.Sum(nil))
}

// oneBankFixture is a flat 1 store with one repo bank to merge.
func oneBankFixture(t *testing.T) string {
	t.Helper()
	return migFixture(t, map[string]string{
		".layout":                  "flat 1\n",
		".origins":                 "alpha\tssh://x\n",
		"decisionLog.md":           "# Decision Log\n\n## 2026-11-10 — home <!-- id:aaaa0001 -->\nbody\n",
		"alpha--decisionLog.md":    "# Decision Log\n\n## 2026-11-12 — new <!-- id:bbbb0002 -->\nb\n\n## 2026-11-10 — dup <!-- id:aaaa0001 -->\nx\n\n## 2026-11-01 — old\nc\n",
		"progress.md":              "# Progress\n\n## What Works\n- home\n\n## In Progress\n",
		"alpha--progress.md":       "# Progress\n\n## What Works\n- shipped\n\n## In Progress\n\n## Known Issues\n",
		"alpha--projectContext.md": "# Project Context\n\n> q\n\n(Describe the project in a line or two)\n",
		"alpha--topic.md":          "topic\n",
		"MEMORY.md": "<!-- bank:begin x -->\n- keep\n<!-- bank:end -->\n## alpha\n- [t](alpha--topic.md) — a\n" +
			"- [t](alpha--topic.md) — dup\n- [p](alpha--progress.md)\n",
	})
}

func TestMigrateMergesIntoOneBank(t *testing.T) {
	dir := oneBankFixture(t)
	var out bytes.Buffer
	if err := Migrate(MigrateOptions{DryRun: true, Out: &out}); err != nil || !strings.Contains(out.String(), "topics renamed 1") {
		t.Fatalf("dry-run: %v %q", err, out.String())
	}
	if err := Migrate(MigrateOptions{Out: &out}); err != nil {
		t.Fatal(err)
	}
	read := func(rel string) string { b, _ := os.ReadFile(filepath.Join(dir, rel)); return string(b) }
	dl := read("decisionLog.md")
	if strings.Index(dl, "new <!-- id:bbbb0002 -->") >= strings.Index(dl, "home") ||
		strings.Index(dl, "home") >= strings.Index(dl, "old") || strings.Count(dl, "id:aaaa0001") != 1 {
		t.Errorf("decisionLog: %q", dl)
	}
	if !strings.Contains(read("progress.md"), "- shipped") || strings.Contains(read("progress.md"), "Known Issues") {
		t.Errorf("progress: %q", read("progress.md"))
	}
	if read("projectContext.md") != "" {
		t.Error("template projectContext not dropped")
	}
	if read("notes/topic.md") != "topic\n" || read("topic.md") != "" || read(".layout") != layoutFlat+"\n" {
		t.Error("topic or layout")
	}
	want := []string{"- [t](topic.md) — a", "- [t](topic.md) — dup", "- [p](progress.md)"}
	if got := testDecode(read("index.md")); !slices.Equal(got, want) {
		t.Errorf("index.md = %q, want each entry repointed past the renames", got)
	}
	for _, gone := range []string{".origins", "alpha--progress.md", "alpha--topic.md"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); err == nil {
			t.Errorf("%s still present", gone)
		}
	}
	if st, _ := migGit(dir, "status", "--porcelain"); st != "" {
		t.Errorf("dirty after commit: %q", st)
	}
	out.Reset()
	if err := Migrate(MigrateOptions{Out: &out}); err != nil || !strings.Contains(out.String(), "already migrated") {
		t.Errorf("re-run: %v %q", err, out.String())
	}
}

func TestMigrateRefusalChangesNothing(t *testing.T) {
	secret := "sk-" + strings.Repeat("a1B2", 8)
	dir := migFixture(t, map[string]string{
		".layout": "flat 1\n", "alpha--decisionLog.md": "## 2026-11-12 — leak\n" + secret + "\n", "handoffs/alpha--h.md": "h\n",
	})
	if err := os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := migHash(t, dir)
	var out bytes.Buffer
	if err := Migrate(MigrateOptions{Out: &out}); !errors.Is(err, ErrMigrateBlocked) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out.String(), "dirty") || !strings.Contains(out.String(), "credential-shaped") {
		t.Errorf("blockers: %q", out.String())
	}
	if migHash(t, dir) != before {
		t.Error("store changed despite refusal")
	}
}

func TestMigrateAppendsRepointedEntriesAndRewritesNoIndexFile(t *testing.T) {
	const hook = " — see handoffs/claude-code--2026-11-01-a.md and knowledge/claude-code--tip.md"
	ptr := func(h, k, w string) []string {
		return []string{"- [h](handoffs/" + h + ")" + hook, "- [k](knowledge/" + k + ")" + hook, "- [w](worktrees/" + w + "/x.md)" + hook}
	}
	old := ptr("claude-code--2026-11-01-a.md", "claude-code--tip.md", "claude-code--2026-09-09-dir")
	cat := "# c\n\n" + encodeRecord(old[0])
	dir := migFixture(t, map[string]string{
		".layout": "flat 2\n", "MEMORY.md": strings.Join(old[1:], "\n") + "\n", "index.md": cat,
		"handoffs/claude-code--2026-11-01-a.md": "handoff\n", "knowledge/claude-code--tip.md": "tip\n",
		"worktrees/claude-code--2026-09-09-dir/x.md": "x\n",
	})
	mem := readRel(dir, "MEMORY.md")
	if err := Migrate(MigrateOptions{}); err != nil {
		t.Fatal(err)
	}
	if readRel(dir, "MEMORY.md") != mem || !strings.HasPrefix(readRel(dir, "index.md"), cat) {
		t.Error("migrate must leave MEMORY.md to the next refresh and only append to index.md")
	}
	want := append([]string{old[0]}, ptr("2026-11-01-a.md", "tip.md", "2026-09-09-dir")...)
	if got := testDecode(readRel(dir, "index.md")); !slices.Equal(got, want) {
		t.Errorf("index.md = %q, want the stored entry, then each entry repointed with its hook text kept", got)
	}
}
