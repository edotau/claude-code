package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
		_, _ = io.WriteString(out, "sync-pre x\nimported: 0 merged, 1 copied, 0 kept local, 0 conflict(s), 0 skipped\nsync-post x\n")
		if st.pushExit != nil {
			return "", st.pushExit(script), nil
		}
		return "", 0, nil
	}
	fetchRoot = func(host string) (string, error) {
		st.calls = append(st.calls, "pull "+host)
		dir := t.TempDir()
		syncMust(t, os.WriteFile(filepath.Join(dir, ".layout"), []byte("flat 3\n"), 0o600))
		return dir, nil
	}
}

func syncMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// syncStore is an opted-in temp config with an initialized store holding one local note.
func syncStore(t *testing.T) {
	t.Helper()
	tempConfig(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("NETWORK_MEMORY", "on")
	t.Setenv("CLAUDE_REMOTE_HOST", "")
	_, err := Init()
	syncMust(t, err)
	syncMust(t, StoreRepo().Ensure())
	write(t, filepath.Join(paths.MemoryStoreDir(), "notes", "local.md"), "local\n")
}

func probeOut(layout string) string { return layout + "\n" + syncTestHead + "\n       0\n" }

type syncCase struct {
	name, hosts, layout, down string // hosts: comma-joined peers, else peer.example; down: must be recorded
	state                     func() *PeerSync
	pushExit                  func(script string) int
	code                      int
	calls                     []string
}

func TestSyncPassOrder(t *testing.T) {
	probe, tok := "probe peer.example", syncTestHead+" 0"
	cases := []syncCase{
		{name: "one peer unreachable", hosts: "peer.example,down.peer.example", layout: "flat 3", down: "down.peer.example",
			calls: []string{probe, "probe down.peer.example", "pull peer.example", "push peer.example"}},
		{name: "held refusal", layout: "flat 3", code: 1, calls: []string{probe}, state: func() *PeerSync {
			return &PeerSync{Pull: SyncDir{Refusal: "credential-shaped content: x.md", Since: time.Now(), Token: tok},
				Push: SyncDir{Refusal: "exit 1: credential-shaped content: y.md", Since: time.Now(), Token: LocalToken()}}
		}},
		{name: "unequal layout", layout: "flat 2", code: 1, calls: []string{probe}},
		{name: "replace refused by an older peer", layout: "flat 3", calls: []string{probe, "push peer.example --replace", "push peer.example"},
			state: func() *PeerSync {
				return &PeerSync{Pulled: tok, Seen: map[string]string{"notes/local.md": strings.Repeat("a", 64)}}
			},
			pushExit: func(s string) int { return map[bool]int{true: 2}[strings.Contains(s, "--replace")] }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { runSyncCase(t, c) })
	}
}

func runSyncCase(t *testing.T, c syncCase) {
	peers := syncCaseSetup(t, c)
	st := &syncStub{probe: map[string]string{"peer.example": probeOut(c.layout)}, pushExit: c.pushExit}
	stubSyncSeams(t, st)
	var out bytes.Buffer
	code, err := RunSync(peers, false, &out)
	syncMust(t, err)
	if code != c.code || !slices.Equal(st.calls, c.calls) {
		t.Fatalf("code %d, calls %v; want %d, %v\n%s", code, st.calls, c.code, c.calls, out.String())
	}
	if p := LoadSync().Peers[c.down]; c.down != "" && (p == nil || p.Unreachable.IsZero()) {
		t.Fatalf("the unreachable peer was not recorded: %+v", p)
	}
}

// syncCaseSetup builds the case's store and stored state, and returns the peer list.
func syncCaseSetup(t *testing.T, c syncCase) []string {
	syncStore(t)
	peers := []string{"peer.example"}
	if c.hosts != "" {
		peers = strings.Split(c.hosts, ",")
	}
	if c.state != nil {
		syncMust(t, SaveSync(&SyncState{Peers: map[string]*PeerSync{"peer.example": c.state()}}))
	}
	return peers
}

func TestSyncRefusesOptionShapedPeer(t *testing.T) {
	syncStore(t)
	st := &syncStub{}
	stubSyncSeams(t, st)
	if code, err := RunSync([]string{"-oProxyCommand=x"}, false, io.Discard); err != nil || code != 2 || len(st.calls) != 0 {
		t.Fatalf("code %d, err %v, calls %v; want 2 before any seam call", code, err, st.calls)
	}
}

var syncLiveBin = flag.String("sync-live-bin", "", "built claude-code binary: run the two-store live sync check")

// syncLiveRun runs script as the "peer": the built binary, config dir peerCfg, local bash, no network.
func syncLiveRun(peerCfg string, script string, in io.Reader, out io.Writer) (string, int, error) {
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+peerCfg, UpdateChildEnv+"=1")
	var stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, &stderr
	err := cmd.Run()
	if x, ok := err.(*exec.ExitError); ok {
		return stderr.String(), x.ExitCode(), nil
	}
	return stderr.String(), 0, err
}

// TestSyncLiveTwoStores is the live check: go test -run TestSyncLiveTwoStores ./internal/memory -args -sync-live-bin=BIN
func TestSyncLiveTwoStores(t *testing.T) {
	if *syncLiveBin == "" {
		t.Skip("needs -sync-live-bin")
	}
	syncStore(t)
	peerCfg := t.TempDir()
	commit := syncLiveSetup(t, peerCfg)
	local := filepath.Join(paths.MemoryStoreDir(), "notes", "local.md")
	peerNote := filepath.Join(peerCfg, "memory", "notes", "local.md")
	steps := []struct{ name, a, b string }{
		{"seed", "", ""}, {"edit on A", "edit A\n", ""}, {"edit both", "A2\n", "B2\n"}, {"no change", "", ""},
	}
	for _, s := range steps {
		for cfg, edit := range map[string][2]string{paths.ConfigDir(): {local, s.a}, peerCfg: {peerNote, s.b}} {
			if edit[1] != "" { // a store writer commits its edit; the token is HEAD plus the dirty-line count
				syncMust(t, os.WriteFile(edit[0], []byte(edit[1]), 0o600))
				_, _, _ = syncLiveRun(cfg, commit, nil, io.Discard)
			}
		}
		var out bytes.Buffer
		code, _ := RunSync([]string{"peer.example"}, false, &out)
		b, _ := os.ReadFile(peerNote)
		t.Logf("== %s: exit %d; peer note %d bytes, equal to A's: %v\n%s",
			s.name, code, len(b), syncFileEqual(local, peerNote), out.String())
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
		_, err := ExtractStoreTar(&tarball, dir)
		return dir, err
	}
	return commit
}

func syncFileEqual(a, b string) bool {
	x, _ := os.ReadFile(a)
	y, _ := os.ReadFile(b)
	return bytes.Equal(x, y)
}

// twoStores is an in-process peer: a second config dir served through syncSSH and fetchRoot, no shell, no network.
type twoStores struct {
	t           *testing.T
	local, peer string              // config dirs
	mangle      func(string) string // rewrites the peer's push answer
	edit        func()              // runs in the second peer's fetch: a local edit between the pull and the push
}

func newTwoStores(t *testing.T) *twoStores {
	syncStore(t)
	ts := &twoStores{t: t, local: paths.ConfigDir(), peer: t.TempDir()}
	ts.asPeer(func() { _, err := Init(); syncMust(t, err); syncMust(t, StoreRepo().Ensure()) })
	for _, cfg := range []string{ts.local, ts.peer} {
		git := []string{"-C", filepath.Join(cfg, "memory"), "-c", "user.name=t", "-c", "user.email=t@peer.example"}
		for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", "seed"}} {
			if b, err := exec.Command("git", append(git, args...)...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v %s", args, err, b)
			}
		}
	}
	oldSSH, oldFetch := syncSSH, fetchRoot
	t.Cleanup(func() { syncSSH, fetchRoot = oldSSH, oldFetch })
	syncSSH, fetchRoot = ts.ssh, ts.fetch
	return ts
}

func (ts *twoStores) asPeer(fn func()) {
	_ = os.Setenv("CLAUDE_CONFIG_DIR", ts.peer)
	defer func() { _ = os.Setenv("CLAUDE_CONFIG_DIR", ts.local) }()
	fn()
}

// ssh answers the probe and the push for peer.example; any other host answers its probe, then is cut off.
func (ts *twoStores) ssh(_ context.Context, host, script string, in io.Reader, out io.Writer) (string, int, error) {
	if host != "peer.example" {
		_, _ = io.WriteString(out, map[bool]string{true: "flat 3\n" + syncTestHead + "\nother\n"}[script == syncProbeScript])
		return "", map[bool]int{false: sshFailExit}[script == syncProbeScript], nil
	}
	code := 0
	ts.asPeer(func() {
		if script == syncProbeScript {
			layout, _ := os.ReadFile(filepath.Join(paths.MemoryStoreDir(), ".layout"))
			head, digest, _ := strings.Cut(LocalToken(), " ")
			_, _ = fmt.Fprintf(out, "%s\n%s\n%s\n", strings.TrimSpace(string(layout)), head, digest)
			return
		}
		code = ts.peerImport(script, in, out)
	})
	return "", code, nil
}

// peerImport is the push script in Go: unpack, import with the .replace list, bracket the answer with tokens.
func (ts *twoStores) peerImport(script string, in io.Reader, out io.Writer) int {
	dir := ts.t.TempDir()
	if _, err := ExtractStoreTar(in, dir); err != nil {
		return 1
	}
	var replace map[string]string
	if strings.Contains(script, "--replace") {
		replace, _ = ReadReplaceList(filepath.Join(dir, ".replace"))
	}
	answer, code := "sync-pre "+LocalToken()+"\n", 0
	res, err := ImportBank(ImportOptions{From: dir, Label: "driver", Topics: true, Replace: replace,
		DryRun: strings.Contains(script, "--dry-run")})
	if err != nil {
		code = 1
	} else {
		answer += strings.Join(append(res.Lines, res.Summary), "\n") + "\n"
	}
	answer += "sync-post " + LocalToken() + "\n"
	if ts.mangle != nil {
		answer = ts.mangle(answer)
	}
	_, _ = io.WriteString(out, answer)
	return code
}

func (ts *twoStores) fetch(host string) (string, error) {
	dir := ts.t.TempDir()
	if host != "peer.example" {
		if ts.edit != nil {
			ts.edit()
		}
		return dir, errors.New("cut off")
	}
	var buf bytes.Buffer
	var err error
	ts.asPeer(func() {
		var rels []string
		for rel := range HashStore() {
			rels = append(rels, rel)
		}
		err = WriteStoreTar(&buf, rels)
	})
	if err == nil {
		_, err = ExtractStoreTar(&buf, dir)
	}
	return dir, err
}

// put writes rel on the local side (peer false) or the peer's.
func (ts *twoStores) put(peer bool, rel, body string) {
	write := func() {
		p := filepath.Join(paths.MemoryStoreDir(), filepath.FromSlash(rel))
		syncMust(ts.t, os.MkdirAll(filepath.Dir(p), 0o700))
		syncMust(ts.t, os.WriteFile(p, []byte(body), 0o600))
	}
	if peer {
		ts.asPeer(write)
		return
	}
	write()
}

func (ts *twoStores) get(rel string) (local, peer string) {
	read := func() string {
		b, _ := os.ReadFile(filepath.Join(paths.MemoryStoreDir(), filepath.FromSlash(rel)))
		return string(b)
	}
	local = read()
	ts.asPeer(func() { peer = read() })
	return local, peer
}

func syncSum(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// step runs one pass and asserts rel's bytes on both sides and its seen entry (one hash, or "local peer").
func (ts *twoStores) step(name, rel, wantLocal, wantPeer string, peers ...string) {
	ts.t.Helper()
	var out bytes.Buffer
	_, _ = RunSync(append([]string{"peer.example"}, peers...), false, &out)
	local, peer := ts.get(rel)
	seen := LoadSync().Peers["peer.example"].Seen[rel]
	want := syncSum(wantLocal)
	if wantLocal != wantPeer {
		want += " " + syncSum(wantPeer)
	}
	if local != wantLocal || peer != wantPeer || seen != want {
		ts.t.Fatalf("%s: local %q peer %q seen %q; want %q %q %q\n%s", name, local, peer, seen, wantLocal, wantPeer, want, out.String())
	}
}

func TestSyncTwoStoresConverge(t *testing.T) {
	ts := newTwoStores(t)
	x, y, logRel := "notes/x.md", "notes/y.md", "decisionLog.md"
	ts.put(false, logRel, "# Log\n\n## 2026-11-01 — local <!-- id:11111111 -->\nLocal body.\n")
	ts.put(true, logRel, "# Log\n\n## 2026-11-02 — peer <!-- id:22222222 -->\nPeer body.\n")
	ts.put(false, x, "a0\n")
	ts.step("seed", x, "a0\n", "a0\n")
	if local, peer := ts.get(logRel); !strings.Contains(peer, "11111111") || !strings.Contains(local, "22222222") {
		t.Fatalf("first sync: logs did not merge both ways:\nlocal %q\npeer %q", local, peer)
	}
	ts.put(false, x, "a1\n")
	ts.step("one-sided local", x, "a1\n", "a1\n")
	ts.put(true, x, "b1\n")
	ts.step("one-sided peer", x, "b1\n", "b1\n")
	ts.put(false, x, "a2\n")
	ts.put(true, x, "b2\n")
	ts.step("both changed", x, "a2\n", "b2\n")
	ts.put(false, x, "a3\n")
	ts.step("local edits again", x, "a3\n", "a3\n")
	ts.put(false, x, "a4\n")
	ts.put(true, x, "b4\n")
	ts.step("both changed again", x, "a4\n", "b4\n")
	ts.put(true, x, "b5\n")
	ts.step("peer edits again", x, "b5\n", "b5\n")
	ts.put(false, x, "a6\n")
	ts.put(true, y, "py\n")
	ts.step("local edit while the peer moved another file", x, "a6\n", "a6\n")
	ts.put(true, x, "b7\n")
	ts.edit = func() { ts.put(false, x, "a7\n") }
	ts.step("local edit between the pull and the push", x, "a7\n", "a7\n", "other.example")
	ts.edit = nil
	ts.put(false, x, "a8\n")
	ts.mangle = func(a string) string { return strings.SplitN(a, "\n", 2)[0] + "\n" + strings.Repeat("x", 1<<17) + "\n" }
	_, _ = RunSync([]string{"peer.example"}, false, io.Discard)
	if seen := LoadSync().Peers["peer.example"].Seen[x]; seen != syncSum("a7\n") {
		t.Fatalf("a truncated answer advanced seen to %q", seen)
	}
	ts.mangle = nil
	ts.step("the next pass records the delivery", x, "a8\n", "a8\n")
}

func TestSyncDryRunWritesNoState(t *testing.T) {
	ts := newTwoStores(t)
	ts.put(false, "notes/x.md", "a\n")
	ts.put(true, "notes/y.md", "p\n")
	var out bytes.Buffer
	_, _ = RunSync([]string{"peer.example"}, true, &out)
	_, peerX := ts.get("notes/x.md")
	localY, _ := ts.get("notes/y.md")
	if _, err := os.Stat(filepath.Join(paths.MemoryStateDir(), "sync.json")); err == nil || peerX != "" || localY != "" {
		t.Fatalf("dry run wrote: state err %v, peer x %q, local y %q\n%s", err, peerX, localY, out.String())
	}
}

func TestSyncBusyIsRetriedNotHeld(t *testing.T) {
	syncStore(t)
	st := &syncStub{probe: map[string]string{"peer.example": probeOut("flat 3")}, pushExit: func(string) int { return ImportBusyExit }}
	stubSyncSeams(t, st)
	for range 2 {
		_, _ = RunSync([]string{"peer.example"}, false, io.Discard)
	}
	if p := LoadSync().Peers["peer.example"]; p.Push.Refusal != "" || countPrefix(st.calls, "push ") != 2 {
		t.Fatalf("busy peer: refusal %q, calls %v; want no hold and a push each pass", p.Push.Refusal, st.calls)
	}
}

// TestSyncProbeDigestTracksContent runs the probe as the peer would: an edit to a dirty file must move its token.
func TestSyncProbeDigestTracksContent(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh: the probe runs on a POSIX peer")
	}
	newTwoStores(t)
	probe := func() string {
		cmd := exec.Command(sh, "-c", syncProbeScript)
		cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+paths.ConfigDir())
		b, err := cmd.Output()
		syncMust(t, err)
		return string(b)
	}
	note := filepath.Join(paths.MemoryStoreDir(), "notes", "local.md")
	syncMust(t, os.WriteFile(note, []byte("one\n"), 0o600))
	a := probe()
	syncMust(t, os.WriteFile(note, []byte("two\n"), 0o600))
	if b := probe(); a == b || b != probe() || strings.Count(b, "\n") != 3 {
		t.Fatalf("probe %q then %q: want three lines that move with the bytes and nothing else", a, b)
	}
}
