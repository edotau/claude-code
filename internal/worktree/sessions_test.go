package worktree

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func TestScanClaimsReportsLiveLedgerErrors(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := os.MkdirAll(ClaimsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	unreadable := filepath.Join(ClaimsDir(), "101-open.jsonl")
	if err := os.Symlink(unreadable, unreadable); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(ClaimsDir(), "102-scan.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ClaimsDir(), "103-readable.jsonl"), []byte("{\"path\":\"dirty.go\",\"agent_id\":\"worker\",\"ts\":1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var got []string
	alive := func(int) bool { return true }
	err := ScanClaims(alive, func(pid int, slug string, c Claim) {
		got = append(got, Label(pid, slug, c.Agent)+":"+c.Path)
	})
	for _, want := range []error{syscall.ELOOP, syscall.EISDIR} {
		if !errors.Is(err, want) {
			t.Errorf("ScanClaims error = %v, want live ledger failure %v", err, want)
		}
	}
	label := "leaf worker of session readable (pid 103)"
	if !reflect.DeepEqual(got, []string{label + ":dirty.go"}) {
		t.Errorf("ScanClaims claims = %v, want readable owner's dirty.go claim", got)
	}
	held, err := holders(alive, []string{"dirty.go"})
	if !reflect.DeepEqual(held["dirty.go"], []string{label}) {
		t.Errorf("holders = %v, want readable owner's claim despite ledger errors", held)
	}
	if !errors.Is(err, syscall.ELOOP) || !errors.Is(err, syscall.EISDIR) {
		t.Errorf("holders error = %v, want the live ledger failures", err)
	}
}

// TestDrainDeliversNewestFiveOnceAndRemovesInbox: six appends drain as the newest five in order, then nothing.
func TestDrainDeliversNewestFiveOnceAndRemovesInbox(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for i := range 6 {
		if err := Append("Sess-1", Notice{TS: int64(i), From: "a", Kind: "conflict", Text: fmt.Sprint("n", i)}); err != nil {
			t.Fatal(err)
		}
	}
	got := Drain("Sess-1")
	if len(got) != drainMax || got[0].Text != "n1" || got[4].Text != "n5" || got[4].Schema != noticeSchema {
		t.Fatalf("drain = %+v, want n1..n5 newest last", got)
	}
	if again := Drain("Sess-1"); len(again) != 0 {
		t.Fatalf("second drain = %+v, want none: the first must remove the inbox", again)
	}
}

// TestNoticesIsolateSessions: the inbox is 0600, another session's drain is empty, an empty session is refused.
func TestNoticesIsolateSessions(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := Append("a", Notice{Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(inbox("a")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("inbox stat = %v, %v; want mode 0600", st, err)
	}
	if Drain("b") != nil || Append("", Notice{Text: "x"}) == nil {
		t.Fatal("another session's drain must be empty and an empty session must be refused")
	}
}

// TestDrainKeepsNoticesAfterOversizedLine: a notice over 1MiB must not drop the ones after it.
func TestDrainKeepsNoticesAfterOversizedLine(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, text := range []string{"A", strings.Repeat("x", 3<<19), "C"} {
		if err := Append("big", Notice{Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	got := Drain("big")
	if len(got) != 3 || got[0].Text != "A" || got[2].Text != "C" {
		t.Fatalf("drain returned %d notices, want 3 (A, oversized, C)", len(got))
	}
}
