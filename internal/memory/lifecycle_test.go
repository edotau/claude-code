package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// decisionLogOver writes n decisionLog.md entries (newest-first), each carrying enough distinct
// tokens to survive minEntryTokens and never near-dup each other.
func decisionLogOver(t *testing.T, path string, n int) {
	t.Helper()
	var b strings.Builder
	b.WriteString("# Decision Log\n")
	for i := n; i >= 1; i-- {
		fmt.Fprintf(&b, "\n## decision %02d\n\nrationale topic%d choice%d\n", i, i, i)
	}
	write(t, path, b.String())
}

func TestPromoteLocalToGlobalMovesOverflow(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	local := Dir(root)
	decisionLogOver(t, filepath.Join(local, "decisionLog.md"), 80)

	n, err := PromoteLocalToGlobal(root, time.Now())
	if err != nil || n == 0 {
		t.Fatalf("promoted %d err %v", n, err)
	}
	kept, _ := os.ReadFile(filepath.Join(local, "decisionLog.md"))
	if lineCount(string(kept)) > FileCaps["decisionLog.md"] {
		t.Errorf("local decisionLog still over cap: %d lines", lineCount(string(kept)))
	}
	global, err := os.ReadFile(filepath.Join(HomeBankDir(), "decisionLog.md"))
	if err != nil || !strings.Contains(string(global), "decision 01") {
		t.Errorf("oldest entry did not land in the global bank: %v %q", err, global)
	}
	// Idempotent: a second run with nothing new over cap promotes nothing.
	if n2, err := PromoteLocalToGlobal(root, time.Now()); err != nil || n2 != 0 {
		t.Errorf("second promote: n=%d err=%v, want a no-op", n2, err)
	}
}

func TestPromoteRefusesCredentialShapedOverflow(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	local := Dir(root)
	decisionLogOver(t, filepath.Join(local, "decisionLog.md"), 80)
	// Plant a credential-shaped value in what will be the OLDEST (overflowing) block.
	data, _ := os.ReadFile(filepath.Join(local, "decisionLog.md"))
	tainted := strings.Replace(string(data), "decision 01", "decision 01\n\ntoken sk-abcdefghijklmnop leaked", 1)
	write(t, filepath.Join(local, "decisionLog.md"), tainted)

	if _, err := PromoteLocalToGlobal(root, time.Now()); err == nil || !strings.Contains(err.Error(), "credential") {
		t.Fatalf("want a credential refusal, got %v", err)
	}
	if _, err := os.ReadFile(filepath.Join(HomeBankDir(), "decisionLog.md")); err == nil {
		t.Error("a refused promote must not have written anything to the global bank")
	}
}

func TestArchiveSettledOverflowOnlyWhenLocalIsGlobal(t *testing.T) {
	cfg := tempConfig(t)
	decisionLogOver(t, filepath.Join(cfg, "docs", "memory", "bank", "decisionLog.md"), 80)
	root := repoRoot(t, "proj") // an ordinary project bank — promote owns its overflow, not archive
	decisionLogOver(t, filepath.Join(Dir(root), "decisionLog.md"), 80)

	if n, err := ArchiveSettledOverflow(root, time.Now()); err != nil || n != 0 {
		t.Errorf("project bank: archive must no-op, got n=%d err=%v", n, err)
	}
	n, err := ArchiveSettledOverflow(cfg, time.Now())
	if err != nil || n == 0 {
		t.Fatalf("global bank: archived %d err %v", n, err)
	}
	arch, err := os.ReadFile(filepath.Join(HomeBankDir(), "archive", "decisionLog-"+time.Now().Format("2006-01")+".md"))
	if err != nil || !strings.Contains(string(arch), "decision 01") {
		t.Errorf("settled entries did not land in the archive: %v", err)
	}
}

func TestDoctorFlagsOversizedAndStale(t *testing.T) {
	tempConfig(t)
	decisionLogOver(t, filepath.Join(HomeBankDir(), "decisionLog.md"), 80)
	old := time.Now().AddDate(0, 0, -(DefaultStaleDays + 1))
	if err := os.Chtimes(filepath.Join(HomeBankDir(), "decisionLog.md"), old, old); err != nil {
		t.Fatal(err)
	}
	problems := Doctor(HomeBankDir(), DefaultStaleDays, time.Now())
	joined := strings.Join(problems, "\n")
	if !strings.Contains(joined, "decisionLog.md: over") {
		t.Errorf("want an over-cap finding, got %v", problems)
	}
	if !strings.Contains(joined, "stale") {
		t.Errorf("want a staleness finding, got %v", problems)
	}
}

func TestDoctorFlagsCredentialShapedValue(t *testing.T) {
	tempConfig(t)
	write(t, filepath.Join(HomeBankDir(), "conventions.md"), "# C\n\n- token AKIAABCDEFGHIJKLMNOP in here\n")
	problems := Doctor(HomeBankDir(), DefaultStaleDays, time.Now())
	if !strings.Contains(strings.Join(problems, "\n"), "possible credential") {
		t.Errorf("want a credential finding, got %v", problems)
	}
}

func TestWriteDoctorNoticeCachesFirstProblem(t *testing.T) {
	tempConfig(t)
	decisionLogOver(t, filepath.Join(HomeBankDir(), "decisionLog.md"), 80)
	if err := WriteDoctorNotice(HomeBankDir(), DefaultStaleDays, time.Now()); err != nil {
		t.Fatal(err)
	}
	notice := mustRead(t, DoctorNoticePath())
	if !strings.Contains(notice, "decisionLog.md: over") {
		t.Errorf("notice %q", notice)
	}
	// A clean bank caches an EMPTY notice — distinct from no file at all.
	write(t, filepath.Join(HomeBankDir(), "decisionLog.md"), "# D\n")
	if err := WriteDoctorNotice(HomeBankDir(), DefaultStaleDays, time.Now()); err != nil {
		t.Fatal(err)
	}
	if notice := mustRead(t, DoctorNoticePath()); notice != "" {
		t.Errorf("want an empty cached notice, got %q", notice)
	}
}

func TestLogAccessAndAccessStats(t *testing.T) {
	dir := t.TempDir()
	day1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	LogAccess(dir, day1, "bank:decisionLog.md", "prompt-recall inject")
	LogAccess(dir, day1.AddDate(0, 0, 1), "bank:decisionLog.md", "prompt-recall inject")
	LogAccess(dir, day1.AddDate(0, 0, 2), "bank:decisionLog.md", "prompt-recall inject")
	LogAccess(dir, day1, "bank:conventions.md", "prompt-recall inject")

	stats, oldest := AccessStats(dir, day1.AddDate(0, 0, 2), 30)
	if oldest.IsZero() || !oldest.Equal(day1) {
		t.Errorf("oldest = %v want %v", oldest, day1)
	}
	if s := stats["bank:decisionLog.md"]; s.Count != 3 || s.Days != 3 {
		t.Errorf("decisionLog stat %+v", s)
	}
	if s := stats["bank:conventions.md"]; s.Count != 1 || s.Days != 1 {
		t.Errorf("conventions stat %+v", s)
	}
}

func TestLogAccessRedactsCredentialShapedReason(t *testing.T) {
	dir := t.TempDir()
	LogAccess(dir, time.Now(), "bank:decisionLog.md", "leaked sk-abcdefghijklmnop in reason")
	data := mustRead(t, filepath.Join(dir, AccessLogFile))
	if strings.Contains(data, "sk-abcdefghijklmnop") {
		t.Error("a credential-shaped reason must be redacted, not logged verbatim")
	}
	if !strings.Contains(data, "redacted") {
		t.Errorf("log %q", data)
	}
}

func TestAccessNominationsHotAndCold(t *testing.T) {
	tempConfig(t)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	dir := HomeBankDir()
	for d := range 5 {
		LogAccess(dir, now.AddDate(0, 0, -d), "bank:decisionLog.md", "prompt-recall inject")
	}
	// Log must cover > coldDays for a cold nomination to fire at all.
	LogAccess(dir, now.AddDate(0, 0, -(coldDays+5)), "bank:progress.md", "prompt-recall inject")

	noms := AccessNominations(now)
	joined := strings.Join(noms, "\n")
	if !strings.Contains(joined, "decisionLog.md: hot") {
		t.Errorf("want a hot nomination, got %v", noms)
	}
	if !strings.Contains(joined, "progress.md: not recalled") {
		t.Errorf("want a cold nomination, got %v", noms)
	}
}

func TestCollectBankStatsAndTimeline(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	_ = WriteHarvestResult("s1", HarvestResult{Status: HarvestOK, Time: time.Now(), BankUpdated: true})
	_ = WriteHarvestResult("s2", HarvestResult{Status: HarvestFailed, Time: time.Now()})
	write(t, filepath.Join(Dir(root), "sessionHistory.md"), "# H\n\n## 2026-01-02 — did work\n")

	s := CollectBankStats()
	if s.Harvests != 2 || s.OK != 1 || s.Failed != 1 || s.BankUpdated != 1 {
		t.Errorf("stats %+v", s)
	}
	report := s.Report()
	if !strings.Contains(report, "success rate: 1/2") {
		t.Errorf("report %q", report)
	}

	entries, sessions := Timeline(root, 10)
	if len(entries) != 2 {
		t.Errorf("timeline entries %+v", entries)
	}
	if len(sessions) != 1 || !strings.Contains(sessions[0], "did work") {
		t.Errorf("timeline sessions %+v", sessions)
	}
}

// The doctor covers the LOCAL bank of the root it is asked about, not only the global one.
func TestDoctorFlagsLocalBankOverCap(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	decisionLogOver(t, filepath.Join(Dir(root), "decisionLog.md"), 80)
	joined := strings.Join(Doctor(root, DefaultStaleDays, time.Now()), "\n")
	if !strings.Contains(joined, "local decisionLog.md: over") {
		t.Errorf("want a local over-cap finding, got %q", joined)
	}
}
