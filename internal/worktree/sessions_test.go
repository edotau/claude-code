package worktree

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"

	"github.com/edotau/claude-code/internal/proc"
)

func BenchmarkReadDirtyScoped2000TrackedFiles(b *testing.B) {
	if _, err := exec.LookPath("git"); err != nil {
		b.Skipf("requires git: %v", err)
	}
	home := b.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "CLAUDE_CONFIG_DIR"} {
		b.Setenv(key, home)
	}
	repo, scoped := readDirtyBenchmarkRepo(b)
	ctx := b.Context()
	top, dirty, err := ReadDirty(ctx, repo, "--", scoped)
	if err != nil || top != repo || len(dirty) != 1 || !dirty[scoped] {
		b.Fatalf("ReadDirty = %q, %v, %v; want only %s", top, dirty, err, scoped)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := ReadDirty(ctx, repo, "--", scoped); err != nil {
			b.Fatal(err)
		}
	}
}

func readDirtyBenchmarkRepo(b *testing.B) (string, string) {
	b.Helper()
	const fileCount, filesPerDir, fileSize = 2000, 50, 4096
	repo := b.TempDir()
	git := func(args ...string) string {
		b.Helper()
		out, err := proc.RunGit(b.Context(), repo, args...)
		if err != nil {
			b.Fatal(err)
		}
		return out
	}
	git("init", "--quiet", "--template=")
	git("config", "core.fsmonitor", "false")
	git("config", "core.untrackedCache", "false")
	content := []byte(strings.Repeat("x", fileSize-1) + "\n")
	var scoped string
	for i := range fileCount {
		dir := filepath.Join(repo, fmt.Sprintf("pkg%02d", i/filesPerDir))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			b.Fatal(err)
		}
		file := filepath.Join(dir, fmt.Sprintf("file%04d.go", i))
		if err := os.WriteFile(file, content, 0o600); err != nil {
			b.Fatal(err)
		}
		if i == fileCount/2 {
			scoped = file
		}
	}
	// An index makes these files tracked without creating a fixture commit.
	git("add", "--", ".")
	if got := strings.Count(git("ls-files", "-z"), "\x00"); got != fileCount {
		b.Fatalf("tracked files = %d, want %d", got, fileCount)
	}
	if err := os.WriteFile(scoped, append(content, []byte("modified\n")...), 0o600); err != nil {
		b.Fatal(err)
	}
	return repo, scoped
}

func TestScanClaimsReportsLiveLedgerErrors(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
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
		got = append(got, fmt.Sprintf("%d-%s-%s:%s", pid, slug, c.Agent, c.Path))
	})
	for _, want := range []error{syscall.ELOOP, syscall.EISDIR} {
		if !errors.Is(err, want) {
			t.Errorf("ScanClaims error = %v, want live ledger failure %v", err, want)
		}
	}
	if !reflect.DeepEqual(got, []string{"103-readable-worker:dirty.go"}) {
		t.Errorf("ScanClaims claims = %v, want readable owner's dirty.go claim", got)
	}
}

// TestDrainDeliversNewestFiveOnceAndRemovesInbox: six appends drain as the oldest five plus a count, then the last.
func TestDrainDeliversNewestFiveOnceAndRemovesInbox(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for i := range 6 {
		if err := Append("Sess-1", Notice{TS: int64(i), From: "a", Kind: "conflict", Text: fmt.Sprint("n", i)}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := DrainErr("Sess-1")
	if err != nil || len(got) != drainMax+1 || got[0].Text != "n0" || got[4].Text != "n4" || got[4].Schema != noticeSchema || got[5].Text != "1 more queued" {
		t.Fatalf("drain = %+v, %v; want n0..n4 oldest first, then 1 more queued", got, err)
	}
	if again, err := DrainErr("Sess-1"); err != nil || len(again) != 1 || again[0].Text != "n5" {
		t.Fatalf("second drain = %+v, want n5", again)
	}
	if again, err := DrainErr("Sess-1"); err != nil || len(again) != 0 {
		t.Fatalf("third drain = %+v, want none: the last must remove the inbox", again)
	}
}

// TestAppendKeepsNewestInboxKeep: an undrained inbox never holds more than inboxKeep notices, the newest ones.
func TestAppendKeepsNewestInboxKeep(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for i := range inboxKeep + 20 {
		if err := Append("lane-x", Notice{TS: int64(i), Text: fmt.Sprint("n", i)}); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(inbox("lane-x"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != inboxKeep || !strings.Contains(lines[0], `"dropped"`) || !strings.Contains(lines[0], "21 dropped") || !strings.Contains(lines[1], `"n21"`) || !strings.Contains(lines[inboxKeep-1], fmt.Sprintf(`"n%d"`, inboxKeep+19)) {
		t.Fatalf("inbox holds %d lines, want a dropped line plus the newest %d (n21..n%d)", len(lines), inboxKeep-1, inboxKeep+19)
	}
}

// TestNoticesIsolateSessions: the inbox is 0600, another session's drain is empty, an empty session is refused.
func TestNoticesIsolateSessions(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := Append("a", Notice{Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(inbox("a")); err != nil || runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("inbox stat = %v, %v; want mode 0600", st, err)
	}
	if got, err := DrainErr("b"); got != nil || err != nil || Append("", Notice{Text: "x"}) == nil {
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
	got, err := DrainErr("big")
	if err != nil || len(got) != 2 || got[0].Text != "A" || got[1].Text != "2 more queued" {
		t.Fatalf("drain returned %d notices, want A then 2 more queued", len(got))
	}
	next, err := DrainErr("big")
	if err != nil || len(next) != 2 || !strings.HasSuffix(next[0].Text, truncatedMark) || next[1].Text != "1 more queued" {
		t.Fatalf("second drain = %d notices, %v; want the truncated oversize then 1 more queued", len(next), err)
	}
}

// TestScanClaimsDropsReusedOwnerPid: a token-stamped file whose pid now runs another process is dead; doubt stays live.
func TestScanClaimsDropsReusedOwnerPid(t *testing.T) {
	const stale, fresh = `{"path":"a.go","ts":1,"owner_start":"T1"}`, `{"path":"b.go","ts":2,"owner_start":"T2"}`
	const legacy = `{"path":"c.go","ts":3}`
	probeErr := errors.New("ps: timed out")
	cases := []struct {
		name, pid, lines string
		token            string
		err              error
		want             []string
		removed          bool
	}{
		{"reused pid", "4242", stale, "T9", nil, nil, true},
		{"pid 1 reads alive by EPERM", "1", stale, "boot", nil, nil, true},
		{"owner exited after the signal probe", "4242", stale, "", proc.ErrNoProcess, nil, true},
		{"live owner, matching token", "4242", stale, "T1", nil, []string{"a.go"}, false},
		{"old-format line, pid reused", "4242", legacy, "T9", nil, []string{"c.go"}, false},
		{"old-format line, pid gone", "4242", legacy, "", proc.ErrNoProcess, []string{"c.go"}, false},
		{"token unreadable", "4242", stale, "", probeErr, []string{"a.go"}, false},
		{"permission denied", "1", stale, "", os.ErrPermission, []string{"a.go"}, false},
		{"resumed on the same pid", "4242", stale + "\n" + fresh, "T2", nil, []string{"a.go", "b.go"}, false},
		{"old line beside a stale one", "4242", legacy + "\n" + stale, "T9", nil, []string{"c.go"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			orig := claimStart
			claimStart = func(int) (string, error) { return tc.token, tc.err }
			t.Cleanup(func() { claimStart = orig })
			file := filepath.Join(ClaimsDir(), tc.pid+"-peer.jsonl")
			if err := os.MkdirAll(ClaimsDir(), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte(tc.lines+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var got []string
			err := ScanClaims(func(int) bool { return true }, func(_ int, _ string, c Claim) { got = append(got, c.Path) })
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ScanClaims = %v, %v; want %v", got, err, tc.want)
			}
			if _, err := os.Stat(file); os.IsNotExist(err) != tc.removed {
				t.Errorf("claims file removed = %v, want %v", os.IsNotExist(err), tc.removed)
			}
		})
	}
}

// writeOwnedClaims files lines (path, token) under pid's claims file for session slug with no dispatch stamp.
func writeOwnedClaims(t *testing.T, pid int, slug string, lines [][2]string) {
	t.Helper()
	if err := os.MkdirAll(ClaimsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, l := range lines {
		fmt.Fprintf(&b, `{"path":%q,"ts":%d,"owner_start":%q}`+"\n", l[0], time.Now().Unix(), l[1])
	}
	if err := os.WriteFile(filepath.Join(ClaimsDir(), fmt.Sprintf("%d-%s.jsonl", pid, slug)), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// liveClaimPaths is the paths LiveClaims names for a dispatched leaf d1 whose CLI process is owner.
func liveClaimPaths(t *testing.T, owner int) map[string][]ClaimHit {
	t.Helper()
	got, err := LiveClaims(ClaimSource{Alive: func(int) bool { return true }, Self: "me", Owner: func() int { return owner },
		Leaf: func(Claim) string { return "leaf" }, Label: func(pid int) string { return fmt.Sprint("pid ", pid) }, Dispatch: "d1"})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestLiveClaimsDispatchOwnProcessClaims: an unstamped claim filed by the caller's own CLI process is its own.
func TestLiveClaimsDispatchOwnProcessClaims(t *testing.T) {
	dir := commitRepo(t)
	orig := claimStart
	claimStart = func(int) (string, error) { return "T2", nil }
	t.Cleanup(func() { claimStart = orig })
	a, b := filepath.Join(dir, "a.txt"), filepath.Join(dir, "b.txt")
	writeOwnedClaims(t, peerPid, "codex-sess", [][2]string{{b, "T1"}, {a, "T2"}})
	if got := liveClaimPaths(t, peerPid+1); len(got[a]) != 1 || len(got[b]) != 1 {
		t.Fatalf("non-owner process: claims %v, want both paths held", got)
	}
	if got := liveClaimPaths(t, peerPid); len(got[a]) != 0 || len(got[b]) != 1 {
		t.Fatalf("own process: claims %v, want a.txt its own and the reused-pid b.txt still held", got)
	}
}

// TestLiveClaimsDispatchKeepsSubagentClaimUnderSamePid: a sibling subagent's claim shares the CLI pid but is not own.
func TestLiveClaimsDispatchKeepsSubagentClaimUnderSamePid(t *testing.T) {
	dir := commitRepo(t)
	claimAs(t, "claude-sess", "sub-2", filepath.Join(dir, "b.txt"))
	if got := liveClaimPaths(t, peerPid); len(got[filepath.Join(dir, "b.txt")]) != 1 {
		t.Fatalf("claims %v, want the subagent's b.txt held", got)
	}
}

// TestDrainOversizeLeaderDoesNotHideQueue: a huge first notice is cut, and the rest stay queued with their count.
func TestDrainOversizeLeaderDoesNotHideQueue(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	texts := []string{strings.Repeat("x", drainBytes*2), "b", "c"}
	for i, text := range texts {
		if err := Append("big-lead", Notice{TS: int64(i), Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := DrainErr("big-lead")
	if err != nil || len(got) != 2 || !strings.HasSuffix(got[0].Text, truncatedMark) || len(got[0].Text) > drainBytes+len(truncatedMark) || got[1].Text != "2 more queued" {
		t.Fatalf("drain = %d notices, %v; want one truncated notice then 2 more queued", len(got), err)
	}
}
