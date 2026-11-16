package memory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// runBank runs fn on a fresh "repo" bank in an isolated flat config.
func runBank(t *testing.T, fn func(t *testing.T, b Bank)) {
	t.Helper()
	isolatedConfig(t)
	b := Bank{Key: "repo"}
	if err := b.Ensure(""); err != nil {
		t.Fatal(err)
	}
	fn(t, b)
}

func TestRotateSessionHistoryRetryDoesNotDuplicateArchive(t *testing.T) {
	runBank(t, func(t *testing.T, b Bank) {
		date := time.Date(2026, time.November, 8, 12, 0, 0, 0, time.UTC)
		entries := make([]string, sessionHistoryMaxEntries+2)
		for i := range entries {
			entries[i] = fmt.Sprintf("## Session %02d\n\nDistinct body %02d.", i, i)
		}
		path := b.Path("sessionHistory.md")
		archive := b.Sub("archive", "sessionHistory-2026-11.md")
		write(t, path, rejoin("# Session history", entries))
		// Recreate an archive write that persisted before the source rewrite failed, plus one still-unarchived entry.
		write(t, archive, rejoin("# Archived sessions", entries[sessionHistoryMaxEntries:sessionHistoryMaxEntries+1]))

		n, err := rotateSessionHistory(b, date)
		if err != nil || n != 2 {
			t.Fatalf("retry rotation = %d, %v; want 2, nil", n, err)
		}
		_, archived := splitHeadingBlocks(mustRead(t, archive))
		if want := entries[sessionHistoryMaxEntries:]; !slices.Equal(archived, want) {
			t.Fatalf("retry duplicated or lost archived entries:\n got: %q\nwant: %q", archived, want)
		}
		if got, want := mustRead(t, path), rejoin("# Session history", entries[:sessionHistoryMaxEntries]); got != want {
			t.Fatalf("retained history:\n got: %q\nwant: %q", got, want)
		}
	})
}

func TestRotateSessionHistoryReportsReadError(t *testing.T) {
	runBank(t, func(t *testing.T, b Bank) {
		if n, err := rotateSessionHistory(b, time.Now()); n != 0 || err != nil {
			t.Fatalf("missing history: got %d, %v", n, err)
		}
		path := b.Path("sessionHistory.md")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		_, readErr := os.ReadFile(path)
		var want *os.PathError
		if !errors.As(readErr, &want) {
			t.Fatalf("fixture did not produce a read error: %v", readErr)
		}
		n, err := rotateSessionHistory(b, time.Now())
		if n != 0 || !errors.Is(err, want.Err) {
			t.Fatalf("unreadable history: got %d, %v; want wrapped %v", n, err, want.Err)
		}
	})
}

// TestSettleBankLockLivesUnderStateNotStore: settleBank locks BankLockPath() under state, not a dotfile in the store.
func TestSettleBankLockLivesUnderStateNotStore(t *testing.T) {
	cfg := tempConfig(t)
	root := repoRoot(t, "proj")
	dir := StoreDir()
	if _, err := settleBank(root, time.Now()); err != nil {
		t.Fatal(err)
	}
	want := BankLockPath()
	if !strings.HasPrefix(want, filepath.Join(cfg, "state")) {
		t.Fatalf("bank lock %s is not under state", want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("lock file missing at %s: %v", want, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".postupdate.lock")); err == nil {
		t.Error("a legacy-shaped lock file was written beside the store")
	}
	if _, err := os.Stat(filepath.Join(dir, ".postupdate")); err == nil {
		t.Error("a legacy-shaped lock file was written beside the store")
	}
}

// fakeCredential builds a credential-shaped string at runtime; it is not a real secret.
func fakeCredential() string { return "sk-" + "ant-api03-" + strings.Repeat("x9Y_", 6) }

// TestSettleBankRefusesCredentialFile: a credential-shaped file in the store is refused without leaking it.
func TestSettleBankRefusesCredentialFile(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	write(t, BankFor(root).Path("leak.md"), "key: "+fakeCredential()+"\n")
	_, err := settleBank(root, time.Now())
	if err == nil || !strings.Contains(err.Error(), "credential-shaped content") {
		t.Fatalf("a credential-shaped file in the store must be refused, got %v", err)
	}
	if strings.Contains(err.Error(), fakeCredential()) {
		t.Fatal("the refusal leaked the credential")
	}
}

// TestSettleDirWrapsTypedFilesWhenALaterStepFails: a failed rotation still leaves every typed file wrapped.
func TestSettleDirWrapsTypedFilesWhenALaterStepFails(t *testing.T) {
	runBank(t, func(t *testing.T, b Bank) {
		long := "- " + strings.Repeat("word ", 40) + "\n"
		write(t, b.Path("activeContext.md"), "# Active\n\n"+long)
		write(t, b.Path("decisionLog.md"), "# Log\n\n## 2026-11-13 — d\n\n"+long)
		entries := make([]string, sessionHistoryMaxEntries+1)
		for i := range entries {
			entries[i] = fmt.Sprintf("## Session %02d\n\n%s", i, long)
		}
		write(t, b.Path("sessionHistory.md"), rejoin("# Session history", entries))
		now := time.Date(2026, time.November, 13, 12, 0, 0, 0, time.UTC)
		if err := os.MkdirAll(b.Sub("archive", "sessionHistory-2026-11.md"), 0o700); err != nil { // the rotation cannot append
			t.Fatal(err)
		}

		if _, err := settleDir(b, now); err == nil {
			t.Fatal("settle succeeded with an unwritable archive")
		}
		files := []string{b.Path("activeContext.md"), b.Path("decisionLog.md"), b.Path("sessionHistory.md")}
		if changed, err := formatFiles(files, true); err != nil || len(changed) != 0 {
			t.Fatalf("settle left unwrapped files %v (err %v)", changed, err)
		}
	})
}

func TestLockPathsKeyOnTheBank(t *testing.T) {
	isolatedConfig(t)
	locks := filepath.Join(StateDir(), "locks")
	if got, want := lockPathOf(HomeBank()), filepath.Join(locks, "home", ".postupdate"); got != want {
		t.Errorf("home lock = %s, want %s", got, want)
	}
	if got, want := lockPathOf(Bank{Key: "repo"}), filepath.Join(locks, "repo", ".postupdate"); got != want {
		t.Errorf("repo lock = %s, want %s", got, want)
	}
	if got, want := lockPathOf(Bank{Key: "home"}), filepath.Join(locks, "repo--home", ".postupdate"); got != want {
		t.Errorf("repo bank keyed home: lock = %s, want %s (never the home bank's)", got, want)
	}
	if got := bankOfStateKey(stateKey(Bank{Key: "home"})); got.Key != "home" {
		t.Errorf("bankOfStateKey(stateKey(repo home)) = %+v, want the repo bank", got)
	}
}

func TestSettleBankCommitsEveryStoreFile(t *testing.T) {
	isolatedConfig(t)
	initStore(t)
	root := gitRepo(t, canonTemp(t, t.TempDir()), "alpha")
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	b := BankFor(root)
	write(t, filepath.Join(StoreDir(), "topic.md"), "a topic\n")
	write(t, b.Sub("reports", "r.md"), "a report\n")
	write(t, b.Path("decisionLog.md"), "# Log\n\n## 2026-11-16 — new entry\nBody.\n")
	if _, err := settleBank(root, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(aliasRead(t, b.Path("decisionLog.md")), "<!-- id:") {
		t.Fatal("entry not indexed")
	}
	for _, name := range []string{"decisionLog.md", "topic.md", "reports--r.md", layoutFile} {
		if dirty, err := StoreRepo().Status(name); err != nil || len(dirty) != 0 {
			t.Errorf("%s remains dirty after settle: %v, %v", name, dirty, err)
		}
	}
}
