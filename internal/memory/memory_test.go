package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func tempConfig(t *testing.T) string {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	return cfg
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// repoRoot makes a git work-tree root (a .git dir is enough for the stat-first check).
func repoRoot(t *testing.T, name string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestDirSlugAndInitNoOverwrite(t *testing.T) {
	cfg := tempConfig(t)
	root := repoRoot(t, "My Repo.v2")
	if got, want := Dir(root), filepath.Join(cfg, "docs", "memory", "bank", "my-repo.v2"); got != want {
		t.Errorf("Dir = %s want %s", got, want)
	}
	if Dir(cfg) != GlobalDir() || !IsGlobal(cfg) || IsGlobal(root) {
		t.Error("the config dir's bank must be the global bank")
	}
	if plain := t.TempDir(); !IsGlobal(plain) {
		t.Errorf("a non-repo root must collapse to the global bank, got %s", Dir(plain))
	}
	dir, err := Init(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range bankFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("missing %s", name)
		}
	}
	write(t, filepath.Join(dir, "activeContext.md"), "mine\n")
	_ = os.Remove(filepath.Join(dir, "progress.md"))
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "activeContext.md")); string(b) != "mine\n" {
		t.Error("Init overwrote an existing file")
	}
	if !HasBank(root) || func() bool { _, err := os.Stat(filepath.Join(dir, "progress.md")); return err != nil }() {
		t.Error("Init did not re-seed a missing file")
	}
}

func TestSplitBlocksFenceAware(t *testing.T) {
	body := "# Conventions\n> note\n\n- first rule\n  - sub\n```\n- not a block\n## nor this\n```\n- second rule\n## Heading\ntext"
	header, blocks := entryBlocks("conventions.md", body)
	if header != "# Conventions\n> note" || len(blocks) != 3 || !strings.Contains(blocks[0], "- not a block") {
		t.Fatalf("header %q blocks %q", header, blocks)
	}
	_, hb := entryBlocks("decisionLog.md", "# D\n## a\n- bullet stays\n~~~\n## fenced\n~~~\n## b")
	if len(hb) != 2 || !strings.Contains(hb[0], "## fenced") {
		t.Errorf("heading blocks %q", hb)
	}
	if got := rejoin(header, blocks[:1]); !strings.HasPrefix(got, "# Conventions\n> note\n\n- first rule") {
		t.Errorf("rejoin %q", got)
	}
}

func TestSearchBanksRanking(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	dir := Dir(root)
	write(t, filepath.Join(dir, "decisionLog.md"), "# D\n\n## 2026-01-01 — router retries on 429\n\nwhy: rate limits\n\n## 2026-01-02 — unrelated choice\n\nthe router is mentioned once\n")
	write(t, filepath.Join(GlobalDir(), "conventions.md"), "# C\n\n- keep commits small\n")
	write(t, filepath.Join(dir, "archive", "sessionHistory-2025-12.md"), "# old\n\n## 2025-12-01 — 429 storm\n")
	hits := SearchBanks(root, "the router 429", 0)
	if len(hits) != 3 || !strings.Contains(hits[0].Block, "retries on 429") || hits[0].Bank != "local" {
		t.Fatalf("hits %+v", hits)
	}
	if hits[0].Title() != "2026-01-01 — router retries on 429" {
		t.Errorf("title %q", hits[0].Title())
	}
	if g := SearchBanks(root, "commits", 5); len(g) != 1 || g[0].Bank != "global" {
		t.Errorf("global hit %+v", g)
	}
	if SearchBanks(root, "the and of", 5) != nil {
		t.Error("all-stopword query must return nothing")
	}
}

func TestRotateAndSettle(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	dir := Dir(root)
	var b strings.Builder
	b.WriteString("# Session History\n\n> newest first\n")
	for i := 35; i >= 1; i-- {
		fmt.Fprintf(&b, "\n## entry %02d\n\n- did %d\n", i, i)
	}
	write(t, filepath.Join(dir, "sessionHistory.md"), b.String())
	now := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	n, err := SettleBank(root, now)
	if err != nil || n != 5 {
		t.Fatalf("rotated %d err %v", n, err)
	}
	_, kept := splitHeadingBlocks(mustRead(t, filepath.Join(dir, "sessionHistory.md")))
	arch := mustRead(t, filepath.Join(dir, "archive", "sessionHistory-2026-09.md"))
	if len(kept) != SessionHistoryMaxEntries || !strings.HasPrefix(kept[0], "## entry 35") || !strings.Contains(arch, "## entry 01") || strings.Contains(arch, "## entry 06") {
		t.Errorf("kept %d first %q archive %q", len(kept), kept[0], arch)
	}
	idx := mustRead(t, filepath.Join(dir, BankIndexFile))
	if !strings.Contains(idx, "## sessionHistory.md (30 entries)") || !strings.Contains(idx, "- entry 35") {
		t.Errorf("index %q", idx)
	}
	if n, _ := RotateSessionHistory(dir, now); n != 0 {
		t.Error("rotation under cap must be a no-op")
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSessionSurfaceOrderAndCaps(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	dir := Dir(root)
	write(t, filepath.Join(dir, "activeContext.md"), "# Active\nfocus")
	write(t, filepath.Join(dir, "progress.md"), "# Progress\nworks")
	write(t, filepath.Join(dir, "decisionLog.md"), "# D\n## d3\n## d2\n## d1\n## d0\n")
	write(t, filepath.Join(dir, "sessionHistory.md"), "# H\n## newest\n## older\n")
	write(t, filepath.Join(GlobalDir(), "conventions.md"), "# C\n- global rule\n")
	s := SessionSurface(root)
	order := []string{"activeContext.md", "progress.md", "decisionLog.md (newest entries)", "## d1", "sessionHistory.md (newest entry)", "## newest", "SHARED global bank", "- global rule"}
	last := -1
	for _, want := range order {
		i := strings.Index(s, want)
		if i <= last {
			t.Fatalf("%q out of order in:\n%s", want, s)
		}
		last = i
	}
	if strings.Contains(s, "## d0") || strings.Contains(s, "## older") {
		t.Error("fallback surface carries only 3 decisions and 1 history entry")
	}
	// A huge pair truncates inside its own budget and never starves the index.
	write(t, filepath.Join(dir, "activeContext.md"), strings.Repeat("— long line of context\n", 2000))
	write(t, filepath.Join(dir, BankIndexFile), "# Memory Bank Index\n- routed entry\n")
	s = SessionSurface(root)
	if n := len([]rune(s)); n > TotalCapChars {
		t.Errorf("surface %d chars > %d", n, TotalCapChars)
	}
	if !strings.Contains(s, "- routed entry") || !strings.Contains(s, "(surface truncated)") {
		t.Errorf("index starved or no truncation marker:\n%s", s[len(s)-400:])
	}
	// An indexed global bank is a pointer (memory-recall covers it), not an injected index.
	write(t, filepath.Join(GlobalDir(), BankIndexFile), "# Memory Bank Index\n- global routed entry\n")
	if s = SessionSurface(root); strings.Contains(s, "global routed entry") || !strings.Contains(s, "memory-recall hook") {
		t.Errorf("global index injected instead of pointed at:\n%s", s[len(s)-400:])
	}
	if got := CapChars("aaaa\nbbbb", 3); got != "" {
		t.Errorf("a cap below the marker length yields empty, got %q", got)
	}
}

func TestUpdateRecordsOutcome(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	dir, _ := Init(root)
	bin := t.TempDir()
	ok := filepath.Join(bin, "claude-ok")
	write(t, ok, "#!/bin/sh\n[ \"$"+HarvestChildEnv+"\" = 1 ] || exit 3\nsleep 0.01\nprintf '\\n## 2026-09-16 — harvested\\n' >> "+filepath.Join(dir, "sessionHistory.md")+"\n")
	bad := filepath.Join(bin, "claude-bad")
	write(t, bad, "#!/bin/sh\nexit 1\n")
	_ = os.Chmod(ok, 0o755)
	_ = os.Chmod(bad, 0o755)
	_ = WriteHarvestResult("s1", HarvestResult{Status: HarvestPending, Time: time.Now(), Log: "/l.log"})

	if err := Update(UpdateOptions{Claude: ok, CWD: root, Session: "s1"}); err != nil {
		t.Fatal(err)
	}
	r, _ := ReadHarvestResult("s1")
	if r.Status != HarvestOK || !r.BankUpdated || r.Log != "/l.log" || r.NeedsAttention(time.Now()) {
		t.Errorf("success result %+v", r)
	}
	if !strings.Contains(mustRead(t, filepath.Join(dir, BankIndexFile)), "harvested") {
		t.Error("update did not settle/index the bank")
	}
	if err := Update(UpdateOptions{Claude: bad, CWD: root, Session: "s2"}); err == nil {
		t.Error("failing claude must return an error")
	}
	if r, _ := ReadHarvestResult("s2"); r.Status != HarvestFailed || r.BankUpdated || !r.NeedsAttention(time.Now()) {
		t.Errorf("failure result %+v", r)
	}
	if err := Update(UpdateOptions{CWD: root, Session: "s3"}); err == nil {
		t.Error("missing claude must fail")
	}
	stale := HarvestResult{Status: HarvestPending, Time: time.Now().Add(-20 * time.Minute)}
	if !stale.NeedsAttention(time.Now()) {
		t.Error("a pending result past the grace window needs attention")
	}
}

func TestMigrateLegacyIdempotentNoOverwrite(t *testing.T) {
	cfg := tempConfig(t)
	legacy := filepath.Join(cfg, "docs", "memory")
	write(t, filepath.Join(legacy, "conventions.md"), "# C\n- legacy\n")
	write(t, filepath.Join(legacy, "proj", "progress.md"), "# P\nlegacy\n")
	write(t, filepath.Join(legacy, "decisionLog.md"), "# D\nlegacy\n")
	write(t, filepath.Join(GlobalDir(), "decisionLog.md"), "# D\nnew\n")
	n, err := MigrateLegacy()
	if err != nil || n != 2 {
		t.Fatalf("moved %d err %v", n, err)
	}
	if mustRead(t, filepath.Join(GlobalDir(), "proj", "progress.md")) != "# P\nlegacy\n" ||
		!strings.Contains(mustRead(t, filepath.Join(GlobalDir(), "conventions.md")), "legacy") {
		t.Error("legacy entries not moved under bank/")
	}
	if mustRead(t, filepath.Join(GlobalDir(), "decisionLog.md")) != "# D\nnew\n" || mustRead(t, filepath.Join(legacy, "decisionLog.md")) != "# D\nlegacy\n" {
		t.Error("an existing destination was overwritten or its legacy twin removed")
	}
	if n, err := MigrateLegacy(); n != 0 || err != nil {
		t.Errorf("second run moved %d err %v", n, err)
	}
}
