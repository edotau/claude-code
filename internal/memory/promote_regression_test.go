package memory

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Given the home bank already holds the id and the local removal fails, When promoting, Then the home copy survives.
func TestPromoteHomeKeepsExistingCopyOnLocalFailure(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	id := seedConventionBlock(t, root)
	local := filepath.Join(Dir(root), "conventions.md")
	data, _ := os.ReadFile(local)
	write(t, filepath.Join(HomeBankDir(), "conventions.md"), string(data))
	if err := os.Chmod(Dir(root), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(Dir(root), 0o700) })
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}

	if _, err := Promote(root, id, "home", false, time.Now()); err == nil {
		t.Fatal("Promote succeeded despite unwritable local bank")
	}
	home, _ := os.ReadFile(filepath.Join(HomeBankDir(), "conventions.md"))
	if n := strings.Count(string(home), "<!-- id:"+id+" -->"); n != 1 {
		t.Errorf("home holds the block %d times, want the original 1", n)
	}
}

// Given a newer block quotes an older block's marker, When promoting the older id, Then only the older block moves.
func TestPromoteIgnoresQuotedMarker(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	dir := Dir(root)
	write(t, filepath.Join(dir, "decisionLog.md"), "# Decisions\n\n"+
		"## 2026-11-02 — B <!-- id:bbbb2222 -->\n```\nsupersedes <!-- id:aaaa1111 -->\n```\n\n"+
		"## 2026-11-01 — A <!-- id:aaaa1111 -->\nold body\n")

	if _, err := Promote(root, "aaaa1111", "home", false, time.Now()); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	local, _ := os.ReadFile(filepath.Join(dir, "decisionLog.md"))
	if !strings.Contains(string(local), "id:bbbb2222") {
		t.Errorf("quoting block B was removed:\n%s", local)
	}
	home, _ := os.ReadFile(filepath.Join(HomeBankDir(), "decisionLog.md"))
	if strings.Contains(string(home), "id:bbbb2222") || !strings.Contains(string(home), "old body") {
		t.Errorf("home got the wrong block:\n%s", home)
	}
}

// Given a block whose marker is wrapped below stacked newer markers, When promoting its oldest id, Then the whole block moves.
func TestPromoteFindsWrappedStackedMarker(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	dir := Dir(root)
	write(t, filepath.Join(dir, "conventions.md"), "# C\n\n- keep me <!-- id:bbbb2222 -->\n\n"+
		"- long rule that wraps\n  onto a second line <!-- id:cccc3333 -->\n  <!-- id:aaaa1111 -->\n")

	if _, err := Promote(root, "aaaa1111", "home", false, time.Now()); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	local, _ := os.ReadFile(filepath.Join(dir, "conventions.md"))
	if strings.Contains(string(local), "long rule") || !strings.Contains(string(local), "keep me") {
		t.Errorf("source bank not updated correctly:\n%s", local)
	}
	home, _ := os.ReadFile(filepath.Join(HomeBankDir(), "conventions.md"))
	if !strings.Contains(string(home), "onto a second line") {
		t.Errorf("home missing the wrapped block:\n%s", home)
	}
}

// Contention must stop archiveFile before any read/rewrite, not merely lock its final write.
func TestPersistenceArchiveFileHonorsRememberLock(t *testing.T) {
	tempConfig(t)
	shortBankWait(t)
	root := repoRoot(t, "proj")
	path := filepath.Join(Dir(root), "conventions.md")
	body := "# Conventions\n\n- old one\n- old two\n"
	write(t, path, body)
	now := time.Now()
	err := WithBankLock(BankLockWait, func() error {
		_, err := archiveFile(path, 3, now)
		if !errors.Is(err, ErrBankBusy) {
			t.Errorf("archiveFile under Remember's bank lock = %v, want ErrBankBusy", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := aliasRead(t, path); got != body {
		t.Fatal("archiveFile rewrote the bank while its lock was held")
	}
	id, err := Remember(root, "feedback", "concurrent knowledge", now)
	if err != nil {
		t.Fatal(err)
	}
	n, err := archiveFile(path, 3, now)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("fixture did not exercise the archive rewrite")
	}
	combined := aliasRead(t, path) + aliasRead(t, archivePath(filepath.Dir(path), filepath.Base(path), now))
	if !strings.Contains(combined, "id:"+id) {
		t.Error("Remember's entry was lost during archiving")
	}
}

func TestSplitOverflowSurfacesReadErrorNotAbsence(t *testing.T) {
	dir := t.TempDir()
	if _, _, _, err := splitOverflow(filepath.Join(dir, "absent.md"), 5); err != nil {
		t.Errorf("absent file: err = %v, want nil", err)
	}
	bad := filepath.Join(dir, "decisionLog.md")
	if err := os.Mkdir(bad, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := archiveFile(bad, 5, time.Now()); err == nil {
		t.Error("archiveFile on an unreadable file returned nil, want the read error")
	}
}

func TestExtractBlockSurfacesReadError(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "decisionLog.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, _, err := extractBlock(dir, "abcd1234")
	if err == nil || errors.Is(err, errNoBlock) {
		t.Errorf("extractBlock err = %v, want the read error, not errNoBlock", err)
	}
}

// TestPromoteSettleFailureRollsBackBothBanks: a failed settle restores the local and home files; the retry moves once.
func TestPromoteSettleFailureRollsBackBothBanks(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	id := seedConventionBlock(t, root)
	local, home := filepath.Join(Dir(root), "conventions.md"), filepath.Join(HomeBankDir(), "conventions.md")
	localBefore, _ := os.ReadFile(local)
	homeBefore, homeErr := os.ReadFile(home)
	orig := afterPromoteMove
	t.Cleanup(func() { afterPromoteMove = orig })
	afterPromoteMove = lockStoreIndex(t)
	if _, err := Promote(root, id, "home", false, time.Now()); err == nil || !strings.Contains(err.Error(), "settle: ") {
		t.Fatalf("Promote with a failing settle = %v, want an error naming the settle", err)
	}
	localAfter, _ := os.ReadFile(local)
	homeAfter, homeAfterErr := os.ReadFile(home)
	if string(localAfter) != string(localBefore) || string(homeAfter) != string(homeBefore) || (homeErr == nil) != (homeAfterErr == nil) {
		t.Fatalf("failed Promote left the banks changed: local %q -> %q, home %q -> %q", localBefore, localAfter, homeBefore, homeAfter)
	}
	afterPromoteMove = orig
	_ = os.Remove(filepath.Join(StoreDir(), ".git", "index.lock"))
	if _, err := Promote(root, id, "home", false, time.Now()); err != nil {
		t.Fatalf("retry: %v", err)
	}
	marker := "<!-- id:" + id + " -->"
	localAfter, _ = os.ReadFile(local)
	homeAfter, _ = os.ReadFile(home)
	if strings.Contains(string(localAfter), marker) || strings.Count(string(homeAfter), marker) != 1 {
		t.Fatalf("retry: local holds the block = %v, home holds it %d times, want false and 1",
			strings.Contains(string(localAfter), marker), strings.Count(string(homeAfter), marker))
	}
}
