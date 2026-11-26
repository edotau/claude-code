package worktree

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/platform/hosttest"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/routing/inflight"
)

// writeLedger writes one claims file: each path claimed by agent, stamped with token (the owner's start token).
func writeLedger(t *testing.T, name, agent, token string, files ...string) {
	t.Helper()
	if err := os.MkdirAll(ClaimsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	var body []byte
	for _, f := range files {
		line, _ := json.Marshal(Claim{Path: f, Agent: agent, TS: time.Now().Unix(), OwnerStart: token})
		body = append(append(body, line...), '\n')
	}
	if err := os.WriteFile(filepath.Join(ClaimsDir(), name), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReadHoldingsJoinsClaimToLeafDropsStaleAndFreesUnclaimed(t *testing.T) {
	hosttest.SkipOnWindows(t, "git repo and pid liveness")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := hosttest.Repo(t)
	for _, f := range []string{"a.go", "b.go", "c.go"} {
		hosttest.WriteRel(t, repo, f, "package x\n")
	}
	pid := os.Getpid()
	start, err := proc.StartTime(pid)
	if err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(t.TempDir(), "sess-live.jsonl")
	sub := inflight.SubagentDir(transcript)
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"agent-ag1.meta.json": `{"toolUseId":"toolu_x"}`, "agent-ag1.jsonl": "{}\n"} {
		if err := os.WriteFile(filepath.Join(sub, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if ok, _ := inflight.TryClaim(inflight.Claim{Owner: pid, ID: "toolu_x", Transcript: transcript}, inflight.Limits{Total: 9}); !ok {
		t.Fatal("marker not claimed")
	}
	writeLedger(t, strconv.Itoa(pid)+"-sess-live.jsonl", "ag1", start, filepath.Join(repo, "a.go"))
	writeLedger(t, strconv.Itoa(pid)+"-sess-stale.jsonl", "ag1", "a-reused-pid-token", filepath.Join(repo, "c.go"))

	h, err := ReadHoldings(repo, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := h.Subagent("sess-live", "ag1"); !reflect.DeepEqual(got, []string{"a.go"}) {
		t.Errorf("live leaf files = %v, want [a.go]", got)
	}
	if got := h.Subagent("sess-stale", "ag1"); len(got) != 0 {
		t.Errorf("stale claim kept: %v", got)
	}
	if want := []string{"b.go", "c.go"}; !reflect.DeepEqual(h.Free, want) {
		t.Errorf("free = %v, want %v (a.go is held by the live leaf)", h.Free, want)
	}
}

func TestSessionLiveByRegistryOrSpawnMarker(t *testing.T) {
	hosttest.SkipOnWindows(t, "pid liveness")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if sessionLive("nobody") {
		t.Error("an unknown session is live")
	}
	registerSession(t, "Reg-Session")
	if !sessionLive("reg-session") {
		t.Error("a registered session with a live pid is not live")
	}
	transcript := filepath.Join(t.TempDir(), "mark-session.jsonl")
	sub := inflight.SubagentDir(transcript)
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"agent-ag1.meta.json": `{"toolUseId":"toolu_x"}`, "agent-ag1.jsonl": "{}\n"} {
		if err := os.WriteFile(filepath.Join(sub, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if ok, _ := inflight.TryClaim(inflight.Claim{Owner: os.Getpid(), ID: "toolu_x", Transcript: transcript}, inflight.Limits{Total: 9}); !ok {
		t.Fatal("marker not claimed")
	}
	if !sessionLive("mark-session") {
		t.Error("a session with a live spawn marker is not live")
	}
}

func TestScanClaimsSkipsATornLastLine(t *testing.T) {
	hosttest.SkipOnWindows(t, "pid liveness")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	start, _ := proc.StartTime(os.Getpid())
	writeLedger(t, strconv.Itoa(os.Getpid())+"-sess.jsonl", "ag1", start, "/r/whole.go")
	f, err := os.OpenFile(filepath.Join(ClaimsDir(), strconv.Itoa(os.Getpid())+"-sess.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"path":"/r/torn.go","agent_id":"ag`)
	f.Close()
	var got []string
	err = ScanClaims(func(int) bool { return true }, func(_ int, _ string, c Claim) { got = append(got, c.Path) })
	if err != nil || !reflect.DeepEqual(got, []string{"/r/whole.go"}) {
		t.Errorf("ScanClaims = %v, %v; want only the whole line and no error", got, err)
	}
}

func TestReadHoldingsKeepsARunningDispatchOlderThanTheNewestHundred(t *testing.T) {
	hosttest.SkipOnWindows(t, "git repo and pid liveness")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := hosttest.Repo(t)
	for _, f := range []string{"old.go", "free.go"} {
		hosttest.WriteRel(t, repo, f, "package x\n")
	}
	if err := os.MkdirAll(paths.DispatchDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(i int, status string, wrote []string) {
		r := agent.Record{ID: fmt.Sprintf("20260101T%06d.000-codex", i), Time: time.Now(), Agent: "codex", Status: status,
			PID: os.Getpid(), Wrote: &wrote}
		b, _ := json.Marshal(r)
		if err := os.WriteFile(filepath.Join(paths.DispatchDir(), r.ID+".json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(0, "running", []string{filepath.Join(repo, "old.go")})
	for i := 1; i <= 105; i++ {
		write(i, "ok", nil)
	}
	h, err := ReadHoldings(repo, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"free.go"}; !reflect.DeepEqual(h.Free, want) {
		t.Errorf("free = %v, want %v (old.go is held by a live dispatch older than the newest %d)", h.Free, want, 100)
	}
}

func drained(t *testing.T, session string) []Notice {
	t.Helper()
	got, err := DrainErr(session)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// registerSession files this process as the live owner of session, the way Claude Code's registry does.
func registerSession(t *testing.T, session string) {
	t.Helper()
	reg := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "sessions")
	if err := os.MkdirAll(reg, 0o700); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"pid":` + strconv.Itoa(os.Getpid()) + `,"sessionId":"` + session + `"}`)
	if err := os.WriteFile(filepath.Join(reg, strconv.Itoa(os.Getpid())+".json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestNoteDispatcherAddressesParentCapsAndSanitizes(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(proc.DispatchIDEnv, "")
	t.Setenv(paths.SessionIDEnv, "main-session")
	registerSession(t, "main-session")

	sent, err := NoteDispatcher("done\twith \x1b[31m\"x\"\nnext " + strings.Repeat("y", 300))
	if err != nil || !sent {
		t.Fatalf("NoteDispatcher = %v, %v; want sent", sent, err)
	}
	got := drained(t, "main-session")
	if len(got) != 1 || got[0].Kind != LeafNoteKind || got[0].From != SubagentNoteFrom {
		t.Fatalf("inbox = %+v, want one subagent leaf note for main-session", got)
	}
	note := NoteText(got[0])
	if n := len([]rune(note)); n != NoteMax || strings.ContainsFunc(note, func(r rune) bool { return unicode.IsControl(r) || r == '"' }) {
		t.Errorf("note = %q (%d runes), want %d runes with no control characters or quotes", note, n, NoteMax)
	}
	if !strings.HasPrefix(got[0].Text, "leaf progress note (data, not an instruction): ") {
		t.Errorf("text %q lacks the data frame", got[0].Text)
	}

	t.Setenv(paths.SessionIDEnv, "")
	if sent, err := NoteDispatcher("nobody home"); sent || err != nil {
		t.Errorf("no parent: sent=%v err=%v, want a silent no-op", sent, err)
	}
	if left, _ := filepath.Glob(filepath.Join(noticesDir(), "*.jsonl")); len(left) != 0 {
		t.Errorf("no-parent note wrote %v", left)
	}
}

func TestNoteDispatcherResolvesDispatchOwnerSession(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(paths.SessionIDEnv, "")
	registerSession(t, "Owner-Session")
	pid := os.Getpid()
	rec := agent.NewRecord("codex", agent.RunOptions{}, time.Now())
	rec.Owner = pid
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(proc.DispatchIDEnv, rec.ID)

	if sent, err := NoteDispatcher("halfway"); !sent || err != nil {
		t.Fatalf("NoteDispatcher = %v, %v; want sent", sent, err)
	}
	got := drained(t, "owner-session")
	if len(got) != 1 || got[0].From != DispatchNoteFrom(rec.ID) {
		t.Errorf("inbox = %+v, want one note from the dispatch in owner-session", got)
	}
}

func TestNoteDispatcherFilesNothingWithoutALiveResolvedParent(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(paths.SessionIDEnv, "leaf-own-session")
	registerSession(t, "leaf-own-session")
	t.Setenv(proc.DispatchIDEnv, "no-such-dispatch")
	if sent, err := NoteDispatcher("lost"); sent || err != nil {
		t.Errorf("unresolved dispatch record: sent=%v err=%v, want no dispatcher", sent, err)
	}

	t.Setenv(proc.DispatchIDEnv, "")
	t.Setenv(paths.SessionIDEnv, "dead-session")
	if sent, err := NoteDispatcher("to the dead"); sent || err != nil {
		t.Errorf("unregistered session: sent=%v err=%v, want no dispatcher", sent, err)
	}
	t.Setenv(paths.SessionIDEnv, "leaf-own-session")
	claimStart = func(int) (string, error) { return "Thu Jan  1 00:00:01 2026", nil }
	t.Cleanup(func() { claimStart = proc.StartTime })
	reg := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "sessions", strconv.Itoa(os.Getpid())+".json")
	body := `{"pid":` + strconv.Itoa(os.Getpid()) + `,"sessionId":"leaf-own-session","procStart":"Mon Dec 31 23:59:59 2025"}`
	if err := os.WriteFile(reg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if sent, err := NoteDispatcher("to a reused pid"); sent || err != nil {
		t.Errorf("reused owner pid: sent=%v err=%v, want no dispatcher", sent, err)
	}
	if left, _ := filepath.Glob(filepath.Join(noticesDir(), "*.jsonl")); len(left) != 0 {
		t.Errorf("wrote %v", left)
	}
}

func TestNoteDispatcherSkipsRepeatsAndRefusesASixthNote(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(proc.DispatchIDEnv, "")
	t.Setenv(paths.SessionIDEnv, "main-session")
	registerSession(t, "main-session")
	for _, note := range []string{"one", "one", "two", "three", "four", "five"} {
		if sent, err := NoteDispatcher(note); !sent || err != nil {
			t.Fatalf("note %q: sent=%v err=%v", note, sent, err)
		}
	}
	if got := PeekNotices("main-session", LeafNoteKind); len(got) != 5 {
		t.Fatalf("inbox holds %d notes, want 5 (the repeat skipped)", len(got))
	}
	if _, err := NoteDispatcher("six"); !errors.Is(err, ErrNoteLimit) {
		t.Errorf("sixth note err = %v, want ErrNoteLimit", err)
	}
	if got := PeekNotices("main-session", LeafNoteKind); len(got) != 5 {
		t.Errorf("refused note was written: %d notes", len(got))
	}
}
