package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/network/remote"
	"github.com/edotau/claude-code/internal/paths"
)

const syncTestHead = "0123456789abcdef0123456789abcdef01234567"

// syncStub records seam calls; a host missing from probe answers like an unreachable ssh (exit 255).
type syncStub struct {
	calls    []string
	probe    map[string]string
	pushExit func(script string) int
}

func stubSyncSeams(t *testing.T, st *syncStub) {
	t.Helper()
	oldSSH, oldFetch := syncSSH, fetchRoot
	t.Cleanup(func() { syncSSH, fetchRoot = oldSSH, oldFetch })
	syncSSH = func(_ context.Context, host, script string, in io.Reader, out io.Writer) (string, int, error) {
		if script == syncProbeScript {
			st.calls = append(st.calls, "probe "+host)
			o, ok := st.probe[host]
			if !ok {
				return "ssh: connect to host " + host + " port 22: Operation timed out\n", sshFailExit, nil
			}
			_, _ = io.WriteString(out, o)
			return "", 0, nil
		}
		call := "push " + host
		if strings.Contains(script, "--replace") {
			call += " --replace"
		}
		st.calls = append(st.calls, call)
		_, _ = io.Copy(io.Discard, in)
		if st.pushExit != nil {
			return "", st.pushExit(script), nil
		}
		_, _ = io.WriteString(out, "imported: 0 merged, 1 copied, 0 kept local, 0 conflict(s), 0 skipped\n")
		return "", 0, nil
	}
	fetchRoot = func(host string) (string, error) {
		st.calls = append(st.calls, "pull "+host)
		dir := t.TempDir()
		memMust(t, os.WriteFile(filepath.Join(dir, ".layout"), []byte("flat 3\n"), 0o600))
		return dir, nil
	}
}

// syncStore is an opted-in temp config with an initialized store holding one local note.
func syncStore(t *testing.T) {
	t.Helper()
	memCfg(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("NETWORK_MEMORY", "on")
	t.Setenv("CLAUDE_REMOTE_HOST", "")
	memInit(t)
	memMust(t, memory.StoreRepo().Ensure())
	memMust(t, os.MkdirAll(filepath.Join(memory.StoreDir(), "notes"), 0o700))
	memMust(t, os.WriteFile(filepath.Join(memory.StoreDir(), "notes", "local.md"), []byte("local\n"), 0o600))
}

func probeOut(layout string) string { return layout + "\n" + syncTestHead + "\n       0\n" }

type syncCase struct {
	name, hosts, layout, down string // hosts: CLAUDE_REMOTE_HOST, else --host peer.example; down: must be recorded
	off                       bool
	state                     func() *memory.PeerSync
	pushExit                  func(script string) int
	code                      int
	calls                     []string
}

func TestSyncPassOrder(t *testing.T) {
	probe, tok := "probe peer.example", syncTestHead+" 0"
	cases := []syncCase{
		{name: "not opted in", off: true, code: 2},
		{name: "one peer unreachable", hosts: "peer.example,down.peer.example", layout: "flat 3", down: "down.peer.example",
			calls: []string{probe, "probe down.peer.example", "pull peer.example", "push peer.example"}},
		{name: "held refusal", layout: "flat 3", code: 1, calls: []string{probe}, state: func() *memory.PeerSync {
			return &memory.PeerSync{Pull: memory.SyncDir{Refusal: "credential-shaped content: x.md", Since: time.Now(), Token: tok},
				Push: memory.SyncDir{Refusal: "busy", Since: time.Now(), Token: memory.LocalToken()}}
		}},
		{name: "unequal layout", layout: "flat 2", code: 1, calls: []string{probe}},
		{name: "replace refused by an older peer", layout: "flat 3", calls: []string{probe, "push peer.example --replace", "push peer.example"},
			state: func() *memory.PeerSync {
				return &memory.PeerSync{Pulled: tok, Seen: map[string]string{"notes/local.md": strings.Repeat("a", 64)}}
			},
			pushExit: func(s string) int { return map[bool]int{true: 2}[strings.Contains(s, "--replace")] }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { runSyncCase(t, c) })
	}
}

func runSyncCase(t *testing.T, c syncCase) {
	args := syncCaseSetup(t, c)
	st := &syncStub{probe: map[string]string{"peer.example": probeOut(c.layout)}, pushExit: c.pushExit}
	stubSyncSeams(t, st)
	code, out, stderr := memCLI(t, args...)
	if code != c.code || !slices.Equal(st.calls, c.calls) {
		t.Fatalf("code %d, calls %v; want %d, %v\n%s%s", code, st.calls, c.code, c.calls, out, stderr)
	}
	if c.off && !strings.Contains(stderr, syncOffMsg) {
		t.Fatalf("off: stderr %q lacks the opt-in hint", stderr)
	}
	if p := memory.LoadSync().Peers[c.down]; c.down != "" && (p == nil || p.Unreachable.IsZero()) {
		t.Fatalf("the unreachable peer was not recorded: %+v", p)
	}
}

// syncCaseSetup builds the case's store, env and stored state, and returns the command line.
func syncCaseSetup(t *testing.T, c syncCase) []string {
	syncStore(t)
	args := []string{"import", "--sync", "--host", "peer.example"}
	if c.hosts != "" {
		t.Setenv("CLAUDE_REMOTE_HOST", c.hosts)
		args = args[:2]
	}
	if c.off {
		t.Setenv("NETWORK_MEMORY", "")
	}
	if c.state != nil {
		memMust(t, memory.SaveSync(&memory.SyncState{Peers: map[string]*memory.PeerSync{"peer.example": c.state()}}))
	}
	return args
}

func TestSyncRefusesOptionShapedPeer(t *testing.T) {
	syncStore(t)
	st := &syncStub{}
	stubSyncSeams(t, st)
	if code, out, _ := memCLI(t, "import", "--sync", "--host=-oProxyCommand=x"); code != 2 || len(st.calls) != 0 {
		t.Fatalf("code %d, calls %v, out %q; want 2 before any seam call", code, st.calls, out)
	}
}

func TestImportReplaceFile(t *testing.T) {
	syncStore(t)
	note := filepath.Join(memory.StoreDir(), "notes", "local.md")
	src := t.TempDir()
	memMust(t, os.WriteFile(filepath.Join(src, ".layout"), []byte("flat 3\n"), 0o600))
	memMust(t, os.MkdirAll(filepath.Join(src, "notes"), 0o700))
	memMust(t, os.WriteFile(filepath.Join(src, "notes", "local.md"), []byte("remote\n"), 0o600))
	list := filepath.Join(t.TempDir(), "replace")
	for _, c := range []struct{ seen, want string }{
		{"stale\n", "conflict notes/local.md: kept local"},
		{"local\n", "replace notes/local.md"},
	} {
		sum := sha256.Sum256([]byte(c.seen))
		memMust(t, os.WriteFile(list, []byte(hex.EncodeToString(sum[:])+"  notes/local.md\n"), 0o600))
		if out := memCLIOK(t, "import", "--from", src, "--topics", "--replace", list); !strings.Contains(out, c.want) {
			t.Fatalf("seen %q: output %q; want %q", c.seen, out, c.want)
		}
	}
	if b, _ := os.ReadFile(note); string(b) != "remote\n" {
		t.Fatalf("after a matching hash the note holds %q, want the source's", b)
	}
}

var syncLiveBin = flag.String("sync-live-bin", "", "built claude-code binary: run the two-store live sync check")

// syncLiveRun runs script as the "peer": the built binary, config dir peerCfg, local bash, no network.
func syncLiveRun(peerCfg string, script string, in io.Reader, out io.Writer) (string, int, error) {
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+peerCfg, memory.UpdateChildEnv+"=1")
	var stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, &stderr
	err := cmd.Run()
	if x, ok := err.(*exec.ExitError); ok {
		return stderr.String(), x.ExitCode(), nil
	}
	return stderr.String(), 0, err
}

// TestSyncLiveTwoStores is the live check: go test -run TestSyncLiveTwoStores ./internal/cli -args -sync-live-bin=BIN
func TestSyncLiveTwoStores(t *testing.T) {
	if *syncLiveBin == "" {
		t.Skip("needs -sync-live-bin")
	}
	syncStore(t)
	peerCfg := t.TempDir()
	commit := syncLiveSetup(t, peerCfg)
	local := filepath.Join(memory.StoreDir(), "notes", "local.md")
	peerNote := filepath.Join(peerCfg, "memory", "notes", "local.md")
	steps := []struct{ name, a, b string }{
		{"seed", "", ""}, {"edit on A", "edit A\n", ""}, {"edit both", "A2\n", "B2\n"}, {"no change", "", ""},
	}
	for _, s := range steps {
		for cfg, edit := range map[string][2]string{paths.ConfigDir(): {local, s.a}, peerCfg: {peerNote, s.b}} {
			if edit[1] != "" { // a store writer commits its edit; the token is HEAD plus the dirty-line count
				memMust(t, os.WriteFile(edit[0], []byte(edit[1]), 0o600))
				_, _, _ = syncLiveRun(cfg, commit, nil, io.Discard)
			}
		}
		var out bytes.Buffer
		code := runSync([]string{"peer.example"}, false, &out)
		b, _ := os.ReadFile(peerNote)
		t.Logf("== %s: exit %d; peer note %d bytes, equal to A's: %v\n%s",
			s.name, code, len(b), memFileEqual(local, peerNote), out.String())
	}
}

// syncLiveSetup makes the peer store and a first commit on both sides, and points both seams at the peer.
func syncLiveSetup(t *testing.T, peerCfg string) (commit string) {
	t.Helper()
	t.Setenv(remote.BinEnv, *syncLiveBin)
	git := `git -C "$CLAUDE_CONFIG_DIR/memory" `
	commit = git + `add -A && ` + git + `-c user.name=t -c user.email=t@peer.example commit -q --allow-empty -m edit`
	for cfg, script := range map[string]string{peerCfg: *syncLiveBin + " memory init && " + git + "init -q && " + commit, paths.ConfigDir(): commit} {
		if tail, code, err := syncLiveRun(cfg, script, nil, io.Discard); code != 0 || err != nil {
			t.Fatalf("store setup: %d %v %s", code, err, tail)
		}
	}
	oldSSH, oldFetch := syncSSH, fetchRoot
	t.Cleanup(func() { syncSSH, fetchRoot = oldSSH, oldFetch })
	syncSSH = func(_ context.Context, _ string, script string, in io.Reader, out io.Writer) (string, int, error) {
		return syncLiveRun(peerCfg, script, in, out)
	}
	fetchRoot = func(string) (string, error) {
		dir := t.TempDir()
		var tarball bytes.Buffer
		if _, code, err := syncLiveRun(peerCfg, memoryFetchScript, nil, &tarball); code != 0 || err != nil {
			return dir, fmt.Errorf("fetch exit %d: %v", code, err)
		}
		_, err := memory.ExtractStoreTar(&tarball, dir)
		return dir, err
	}
	return commit
}

func memFileEqual(a, b string) bool {
	x, _ := os.ReadFile(a)
	y, _ := os.ReadFile(b)
	return bytes.Equal(x, y)
}
