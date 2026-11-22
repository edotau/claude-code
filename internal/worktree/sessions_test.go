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
		got = append(got, label(pid, slug, c.Agent)+":"+c.Path)
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
	got, err := DrainErr("Sess-1")
	if err != nil || len(got) != drainMax || got[0].Text != "n1" || got[4].Text != "n5" || got[4].Schema != noticeSchema {
		t.Fatalf("drain = %+v, %v; want n1..n5 newest last", got, err)
	}
	if again, err := DrainErr("Sess-1"); err != nil || len(again) != 0 {
		t.Fatalf("second drain = %+v, want none: the first must remove the inbox", again)
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
	if len(lines) != inboxKeep || !strings.Contains(lines[0], fmt.Sprintf(`"n%d"`, 20)) || !strings.Contains(lines[inboxKeep-1], fmt.Sprintf(`"n%d"`, inboxKeep+19)) {
		t.Fatalf("inbox holds %d lines, want the newest %d (n20..n%d)", len(lines), inboxKeep, inboxKeep+19)
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
	if err != nil || len(got) != 3 || got[0].Text != "A" || got[2].Text != "C" {
		t.Fatalf("drain returned %d notices, want 3 (A, oversized, C)", len(got))
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

// TestCommitCLIDispatchOwnProcessClaims: an unstamped claim filed by the caller's own CLI process is its own to commit.
func TestCommitCLIDispatchOwnProcessClaims(t *testing.T) {
	dir := commitRepo(t)
	t.Setenv(proc.DispatchIDEnv, "d1")
	orig := claimStart
	claimStart = func(int) (string, error) { return "T2", nil }
	t.Cleanup(func() { claimStart = orig })
	hosttest.WriteRel(t, dir, "a.txt", "mine\n")
	hosttest.WriteRel(t, dir, "b.txt", "an earlier process at this pid\n")
	writeOwnedClaims(t, peerPid, "codex-sess", [][2]string{{filepath.Join(dir, "b.txt"), "T1"}, {filepath.Join(dir, "a.txt"), "T2"}})
	req := func(file string, owner int) CommitRequest {
		r := commitReq(dir, "fix: mine", file)
		r.Source.Owner = func() int { return owner }
		return r
	}
	if _, err := commitRun(req("a.txt", peerPid+1)); err == nil || !strings.Contains(err.Error(), "a.txt is claimed by") {
		t.Fatalf("non-owner process: err = %v, want a refusal naming a.txt", err)
	}
	if _, err := commitRun(req("b.txt", peerPid)); err == nil || !strings.Contains(err.Error(), "b.txt is claimed by") {
		t.Fatalf("reused pid (token T1, now T2): err = %v, want a refusal naming b.txt", err)
	}
	if _, err := commitRun(req("a.txt", peerPid)); err != nil {
		t.Fatalf("own process's claim refused: %v", err)
	}
}

// TestCommitCLIDispatchRefusesSubagentClaimUnderSamePid: a sibling subagent's claim shares the CLI pid but is not own.
func TestCommitCLIDispatchRefusesSubagentClaimUnderSamePid(t *testing.T) {
	dir := commitRepo(t)
	t.Setenv(proc.DispatchIDEnv, "d1")
	hosttest.WriteRel(t, dir, "b.txt", "sibling's\n")
	claimAs(t, "claude-sess", "sub-2", filepath.Join(dir, "b.txt"))
	r := commitReq(dir, "fix: x", "b.txt")
	r.Source.Owner = func() int { return peerPid }
	if _, err := commitRun(r); err == nil || !strings.Contains(err.Error(), "b.txt is claimed by") {
		t.Fatalf("err = %v, want a refusal naming b.txt", err)
	}
}
