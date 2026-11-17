package memory

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// Contention must stop archiveFile before any read/rewrite, not merely lock its final write.
func TestPersistenceArchiveFileHonorsRememberLock(t *testing.T) {
	tempConfig(t)
	shortBankWait(t)
	path := Path("conventions.md")
	body := "# Conventions\n\n- old one\n- old two\n"
	write(t, path, body)
	now := time.Now()
	err := WithBankLock(BankLockWait, func() error {
		_, err := archiveFile("conventions.md", 3, now)
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
	id, err := Remember("feedback", "concurrent knowledge", now)
	if err != nil {
		t.Fatal(err)
	}
	n, err := archiveFile("conventions.md", 3, now)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("fixture did not exercise the archive rewrite")
	}
	combined := aliasRead(t, path) + aliasRead(t, archivePath("conventions.md", now))
	if !strings.Contains(combined, "id:"+id) {
		t.Error("Remember's entry was lost during archiving")
	}
}

func TestSplitOverflowSurfacesReadErrorNotAbsence(t *testing.T) {
	isolatedConfig(t)
	if _, _, _, err := splitOverflow(Path("absent.md"), 5); err != nil {
		t.Errorf("absent file: err = %v, want nil", err)
	}
	if err := os.MkdirAll(Path("decisionLog.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := archiveFile("decisionLog.md", 5, time.Now()); err == nil {
		t.Error("archiveFile on an unreadable file returned nil, want the read error")
	}
}

func TestExtractBlockSurfacesReadError(t *testing.T) {
	isolatedConfig(t)
	if err := os.MkdirAll(Path("decisionLog.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, _, err := extractBlock("abcd1234")
	if err == nil || errors.Is(err, errNoBlock) {
		t.Errorf("extractBlock err = %v, want the read error, not errNoBlock", err)
	}
}
