package worktree

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

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
