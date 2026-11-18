package worktree

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/proc"
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
	if SessionLive("nobody") {
		t.Error("an unknown session is live")
	}
	registerSession(t, "Reg-Session")
	if !SessionLive("reg-session") {
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
	if !SessionLive("mark-session") {
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
