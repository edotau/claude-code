package memory

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestStripRepoPrefix(t *testing.T) {
	now := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct{ in, rest, key, dated string }{
		{"claude-code--2026-11-01-a.md", "2026-11-01-a.md", "claude-code", "2026-11-01-a.md"},
		{"claude-code--claude-code--a.md", "a.md", "claude-code", "2026-11-16-a.md"},
		{"digitalbtd-rapid--notes.md", "notes.md", "digitalbtd-rapid", "2026-11-16-notes.md"},
		{"2026-11-16-foo--bar.md", "2026-11-16-foo--bar.md", "", "2026-11-16-foo--bar.md"},
		{"claude-code--20261105-x.md", "20261105-x.md", "claude-code", "2026-11-05-x.md"},
		{"claude-code--", "claude-code--", "", "2026-11-16-claude-code--"},
		{"--x.md", "--x.md", "", "2026-11-16---x.md"},
	} {
		if rest, key := StripRepoPrefix(c.in); rest != c.rest || key != c.key {
			t.Errorf("StripRepoPrefix(%q) = %q, %q", c.in, rest, key)
		}
		if got := DatedName(c.in, now); got != c.dated {
			t.Errorf("DatedName(%q) = %q, want %q", c.in, got, c.dated)
		}
	}
}

func readRel(dir, rel string) string {
	b, _ := os.ReadFile(filepath.Join(dir, rel))
	return string(b)
}

func checkRenamedRefs(t *testing.T, dir string) {
	t.Helper()
	if got := readRel(dir, "index.md"); !strings.HasPrefix(got, "handoffs/claude-code--2026-11-01-a.md\n") ||
		!slices.Equal(testDecode(got), []string{"- [h](handoffs/2026-11-01-a.md)"}) {
		t.Errorf("index.md must keep its text and gain the repointed entry: %q", got)
	}
	if got := readRel(dir, "MEMORY.md"); got != "- [h](handoffs--claude-code--2026-11-01-a.md)\n" {
		t.Errorf("MEMORY.md must be left to the next refresh: %q", got)
	}
	if got := readRel(dir, "reports/2026-10-05-audit.md"); strings.Contains(got, "claude-code--") {
		t.Errorf("audit: %q", got)
	}
	plain := readRel(dir, "reports/2026-11-01-plain.md")
	if !strings.Contains(plain, "../handoffs/2026-11-01-a.md and handoffs/2026-11-01-a.md.") {
		t.Errorf("plain: %q", plain)
	}
}

func TestMigrateRenamesFolderEntries(t *testing.T) {
	dir := migFixture(t, map[string]string{
		".layout":                                    "flat 2\n",
		"index.md":                                   "handoffs/claude-code--2026-11-01-a.md\n",
		"MEMORY.md":                                  "- [h](handoffs--claude-code--2026-11-01-a.md)\n",
		"handoffs/claude-code--2026-11-01-a.md":      "handoff\n",
		"reports/claude-code--audit.md":              "# Audit\nrun on 2026-10-05 ok\n",
		"reports/claude-code--plain.md":              "see claude-code--2026-11-01-a.md and handoffs/claude-code--2026-11-01-a.md.\n",
		"knowledge/claude-code--tip.md":              "tip\n",
		"worktrees/claude-code--2026-09-09-dir/x.md": "x\n",
	})
	before := migHash(t, dir)
	var out bytes.Buffer
	if err := Migrate(MigrateOptions{DryRun: true, Out: &out}); err != nil || !strings.Contains(out.String(), "rename handoffs/claude-code--2026-11-01-a.md -> handoffs/2026-11-01-a.md") {
		t.Fatalf("dry-run: %v %q", err, out.String())
	}
	if migHash(t, dir) != before {
		t.Fatal("dry-run wrote")
	}
	head, _ := migGit(dir, "rev-list", "--count", "HEAD")
	if err := Migrate(MigrateOptions{Out: &out}); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"handoffs/2026-11-01-a.md", "reports/2026-10-05-audit.md", "knowledge/tip.md", "worktrees/2026-09-09-dir/x.md"} {
		if readRel(dir, rel) == "" {
			t.Errorf("missing %s", rel)
		}
	}
	checkRenamedRefs(t, dir)
	if readRel(dir, ".layout") != layoutFlat+"\n" {
		t.Error("layout not stamped")
	}
	if n, _ := migGit(dir, "rev-list", "--count", "HEAD"); n == head {
		t.Error("no commit")
	}
	after := migHash(t, dir)
	out.Reset()
	if err := Migrate(MigrateOptions{Out: &out}); err != nil || !strings.Contains(out.String(), "already migrated") || migHash(t, dir) != after {
		t.Errorf("re-run: %v %q", err, out.String())
	}
}

func migBlk(id, title string) string {
	return "## 2026-11-0" + id + " — " + title + " <!-- id:aaaa000" + id + " -->\nbody\n\n"
}

func migRun(t *testing.T, files map[string]string) (string, string, error) {
	files[".layout"] = "flat 2\n"
	dir := migFixture(t, files)
	var out bytes.Buffer
	err := Migrate(MigrateOptions{Out: &out})
	return dir, out.String(), err
}

func TestMigrateRenameCollisions(t *testing.T) {
	run := func(files map[string]string) (string, string, error) { return migRun(t, files) }
	t.Run("identical dropped", func(t *testing.T) {
		dir, _, err := run(map[string]string{"knowledge/k--a.md": "same\n", "knowledge/a.md": "same\n"})
		if err != nil || readRel(dir, "knowledge/k--a.md") != "" || readRel(dir, "knowledge/a.md") != "same\n" {
			t.Errorf("err=%v", err)
		}
	})
	t.Run("different gets key suffix", func(t *testing.T) {
		dir, _, err := run(map[string]string{"knowledge/k--a.md": "one\n", "knowledge/a.md": "two\n"})
		if err != nil || readRel(dir, "knowledge/a-k.md") != "one\n" || readRel(dir, "knowledge/a.md") != "two\n" {
			t.Errorf("err=%v", err)
		}
	})
	t.Run("taken suffix blocks", func(t *testing.T) {
		files := map[string]string{"knowledge/k--a.md": "one\n", "knowledge/a.md": "two\n", "knowledge/a-k.md": "three\n"}
		files[".layout"] = "flat 2\n"
		dir := migFixture(t, files)
		before := migHash(t, dir)
		if err := Migrate(MigrateOptions{Out: &bytes.Buffer{}}); err == nil || migHash(t, dir) != before {
			t.Errorf("err=%v", err)
		}
	})
	t.Run("unreadable archive source blocks", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("chmod 000 does not block reads here")
		}
		files := map[string]string{"archive/k--decisionLog-1.md": migBlk("1", "a"), "archive/decisionLog-1.md": migBlk("2", "b"), ".layout": "flat 2\n"}
		dir := migFixture(t, files)
		src := filepath.Join(dir, "archive/k--decisionLog-1.md")
		if err := os.Chmod(src, 0); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chmod(src, 0o644) }()
		before := migHash(t, dir)
		var out bytes.Buffer
		if err := Migrate(MigrateOptions{Out: &out}); !errors.Is(err, ErrMigrateBlocked) || !strings.Contains(out.String(), "unreadable") || migHash(t, dir) != before {
			t.Errorf("err=%v out=%q", err, out.String())
		}
	})
}

func TestMigrateRenameArchiveCollisions(t *testing.T) {
	run := func(files map[string]string) (string, string, error) { return migRun(t, files) }
	blk := migBlk
	t.Run("archive merges by id", func(t *testing.T) {
		dir, _, err := run(map[string]string{
			"decisionLog.md":                    "# D\n\n" + blk("3", "live"),
			"archive/decisionLog-2026-11.md":    "# D\n\n" + blk("1", "target"),
			"archive/k--decisionLog-2026-11.md": "# D\n\n" + blk("1", "target") + blk("2", "new") + blk("3", "live"),
		})
		got := readRel(dir, "archive/decisionLog-2026-11.md")
		if err != nil || !strings.Contains(got, "id:aaaa0002") || strings.Contains(got, "id:aaaa0003") || strings.Count(got, "id:aaaa0001") != 1 ||
			readRel(dir, "archive/k--decisionLog-2026-11.md") != "" {
			t.Errorf("err=%v %q", err, got)
		}
	})
	t.Run("id-less archive merges by heading", func(t *testing.T) {
		dir, _, err := run(map[string]string{
			"archive/old.md":    "# O\n\n## 2026-09-02 — a\nx\n",
			"archive/k--old.md": "# O\n\n## 2026-09-02 — a\nx\n\n## 2026-09-01 — b\ny\n",
		})
		got := readRel(dir, "archive/old.md")
		if err != nil || !strings.Contains(got, "— b") || strings.Count(got, "— a") != 1 || strings.Contains(got, "DIFFERENT") {
			t.Errorf("err=%v %q", err, got)
		}
	})
	t.Run("bullet archive keeps new paragraphs", func(t *testing.T) {
		dir, _, err := run(map[string]string{
			"archive/c.md":    "# c\n\n- have <!-- id:aaaa0001 -->\n",
			"archive/k--c.md": "# c\n\n- have <!-- id:aaaa0001 -->\n\n- fresh <!-- id:aaaa0002 -->\n",
		})
		got := readRel(dir, "archive/c.md")
		if err != nil || !strings.Contains(got, "fresh") || strings.Count(got, "id:aaaa0001") != 1 {
			t.Errorf("err=%v %q", err, got)
		}
	})
}
