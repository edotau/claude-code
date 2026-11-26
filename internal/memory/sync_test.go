package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// fakeEnvelopes is an in-memory EnvelopeStore: per-origin rows and watermarks, apply records the seq.
type fakeEnvelopes struct {
	rows    map[string][]Envelope
	acked   map[string]int64
	applied []int64
}

func (f *fakeEnvelopes) ChangesAfter(origin string, after int64, limit int) ([]Envelope, error) {
	var out []Envelope
	for _, e := range f.rows[origin] {
		if e.Seq > after && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}
func (f *fakeEnvelopes) Watermark(origin string) (int64, int, error) { return f.acked[origin], 0, nil }
func (f *fakeEnvelopes) ApplyEnvelope(e Envelope) (bool, error) {
	f.applied, f.acked[e.Origin] = append(f.applied, e.Seq), max(f.acked[e.Origin], e.Seq)
	return false, nil
}
func (f *fakeEnvelopes) Origins() (string, []string, error) {
	var all []string
	for o := range f.rows {
		all = append(all, o)
	}
	slices.Sort(all)
	return "self", all, nil
}
func (f *fakeEnvelopes) Conflicts(bool) ([]ConflictRow, error) { return nil, nil }
func (f *fakeEnvelopes) ResolveConflict(int64, string) error   { return nil }
func (f *fakeEnvelopes) Close() error                          { return nil }

func envLine(seq int64, schema int) string {
	return fmt.Sprintf(`{"origin":"peer","origin_seq":%d,"schema":%d,"entry_ref":"u","op":"upsert"}`+"\n", seq, schema)
}

// TestEnvelopeImportHolds: lines apply in seq order; a gap or a schema mismatch holds the origin unacknowledged.
func TestEnvelopeImportHolds(t *testing.T) {
	st := &fakeEnvelopes{acked: map[string]int64{}}
	rep, err := ImportEnvelopes(st, strings.NewReader(envLine(2, 2)+envLine(1, 2)+envLine(4, 2)), 2)
	if err != nil || !slices.Equal(st.applied, []int64{1, 2}) || len(rep.Held) != 1 || !strings.Contains(rep.Held[0], "gap") {
		t.Fatalf("ordered import = %+v, applied %v (%v)", rep, st.applied, err)
	}
	rep, err = ImportEnvelopes(st, strings.NewReader(envLine(3, 3)), 2)
	if err != nil || st.acked["peer"] != 2 || len(rep.Held) != 1 || !strings.Contains(rep.Held[0], "claude-code upgrade") {
		t.Fatalf("newer schema import = %+v, acked %d (%v)", rep, st.acked["peer"], err)
	}
	if _, err := ImportEnvelopes(st, strings.NewReader(`{"origin":"a/b","origin_seq":1,"schema":2}`), 2); err == nil {
		t.Fatal("an origin with a slash was accepted")
	}
}

// TestEnvelopeExportOrder: export pages in seq order and refuses a store that skips a seq.
func TestEnvelopeExportOrder(t *testing.T) {
	st := &fakeEnvelopes{rows: map[string][]Envelope{"peer": {{Origin: "peer", Seq: 1}, {Origin: "peer", Seq: 2}}}}
	var b strings.Builder
	if n, err := ExportEnvelopes(st, "peer", 0, &b); err != nil || n != 2 || strings.Count(b.String(), "\n") != 2 {
		t.Fatalf("export = %d (%v): %q", n, err, b.String())
	}
	st.rows["peer"] = []Envelope{{Origin: "peer", Seq: 3}}
	if _, err := ExportEnvelopes(st, "peer", 1, &b); err == nil {
		t.Fatal("export accepted a skipped seq")
	}
}

func fakeRows(origin string, n int) *fakeEnvelopes {
	f := &fakeEnvelopes{rows: map[string][]Envelope{}, acked: map[string]int64{}}
	for i := int64(1); i <= int64(n); i++ {
		f.rows[origin] = append(f.rows[origin], Envelope{Origin: origin, Seq: i, Schema: 2, EntryRef: "u", Op: EnvelopeOpUpsert})
	}
	return f
}

func TestParseSinceIsTyped(t *testing.T) {
	got, err := ParseSince("a1=3,b-2=0")
	if err != nil || got["a1"] != 3 || got["b-2"] != 0 || FormatSince(got) != "a1=3,b-2=0" {
		t.Fatalf("ParseSince = %v (%v)", got, err)
	}
	for _, bad := range []string{"a=1;rm -rf /", "a=-1", "a", "=1", "a b=1", "a=x", "$(id)=1", "a=1,,b=2", "a=1,a=2"} {
		if _, err := ParseSince(bad); err == nil {
			t.Errorf("ParseSince(%q) accepted", bad)
		}
	}
	if s := FormatSince(map[string]int64{"ok": 1, "bad;origin": 2}); s != "ok=1" {
		t.Errorf("FormatSince kept a non-plain origin: %q", s)
	}
}

func TestServeExportStreamEndsAndCaps(t *testing.T) {
	f := fakeRows("peer", 5)
	var b bytes.Buffer
	if err := ServeExport(f, map[string]int64{"peer": 2}, &b); err != nil {
		t.Fatal(err)
	}
	body, n, err := ReadStream(&b)
	if err != nil || n != 3 || strings.Count(string(body), "\n") != 3 {
		t.Fatalf("stream = %q, %d (%v)", body, n, err)
	}
	b.Reset()
	if err := serveExport(f, nil, &b, 200); err != nil {
		t.Fatal(err)
	}
	if body, n, err = ReadStream(&b); err != nil || n == 0 || n >= 5 || strings.Count(string(body), "\n") != n {
		t.Fatalf("capped stream holds %d (%v): want a nonempty valid prefix", n, err)
	}
}

func TestServeExportRefusesEnvelopeAboveReadCap(t *testing.T) {
	st := fakeRows("peer", 1)
	st.rows["peer"][0].Payload = json.RawMessage(`"` + strings.Repeat("x", streamReadMax) + `"`)
	var out bytes.Buffer
	if err := ServeExport(st, nil, &out); err == nil || out.Len() != 0 {
		t.Fatalf("oversize envelope: wrote %d bytes (%v)", out.Len(), err)
	}
}

func TestPeerOutputIsBoundedBeforeBuffering(t *testing.T) {
	cancelled := false
	b := &limitedBuffer{cancel: func() { cancelled = true }}
	if _, err := b.Write(make([]byte, streamReadMax+1)); err == nil || !cancelled || b.Len() != 0 {
		t.Fatalf("oversize output buffered %d bytes, cancelled %v (%v)", b.Len(), cancelled, err)
	}
}

func TestSyncPeersRefusesOversizePull(t *testing.T) {
	st := fakeRows("local", 0)
	defer StubSyncSSH(func(_ context.Context, _, _ string, _ io.Reader, out io.Writer) (string, int, error) {
		_, err := out.Write(make([]byte, streamReadMax+1))
		return "", 0, err
	})()
	var out strings.Builder
	if got := SyncPeers(context.Background(), st, []string{"peer.example"}, &out); got != 1 || !strings.Contains(out.String(), "over the size cap") {
		t.Fatalf("oversize pull: exit %d, out %q", got, out.String())
	}
}

func TestSyncPeersReportsExpiredDeadlineAsPending(t *testing.T) {
	st := fakeRows("local", 0)
	defer StubSyncSSH(func(ctx context.Context, _, _ string, _ io.Reader, _ io.Writer) (string, int, error) {
		return "", 0, ctx.Err()
	})()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out strings.Builder
	if got := SyncPeers(ctx, st, []string{"peer.example"}, &out); got != 1 || !strings.Contains(out.String(), "pending") {
		t.Fatalf("expired pass: exit %d, out %q", got, out.String())
	}
}

func TestSSHTailBufferKeepsOnlyLastBytes(t *testing.T) {
	var b tailBuffer
	if _, err := b.Write([]byte(strings.Repeat("a", sshTailMax))); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte("end")); err != nil {
		t.Fatal(err)
	}
	if got := string(b.buf); len(got) != sshTailMax || !strings.HasSuffix(got, "end") || !strings.HasPrefix(got, "aaa") {
		t.Fatalf("stderr tail = %d bytes: %q", len(got), got)
	}
}

func TestReadStreamRefusesCutOrMiscounted(t *testing.T) {
	line := envLine(1, 2)
	for name, in := range map[string]string{
		"empty": "", "missing count": `{"end":true}`, "null count": `{"end":true,"count":null}`, "no end": line, "cut end": line + `{"end":true,"co`, "wrong count": line + `{"end":true,"count":2}` + "\n",
		"end not true": line + `{"end":false,"count":1}` + "\n", "extra field": line + `{"end":true,"count":1,"x":1}` + "\n",
		"data after end": line + `{"end":true,"count":1}{"end":true,"count":1}` + "\n",
	} {
		if _, _, err := ReadStream(strings.NewReader(in)); err == nil {
			t.Errorf("%s: stream accepted", name)
		}
	}
	if _, n, err := ReadStream(strings.NewReader(line + `{"end":true,"count":1}` + "\n")); err != nil || n != 1 {
		t.Errorf("good stream = %d (%v)", n, err)
	}
}

func TestImportStreamRequiresAtomicStore(t *testing.T) {
	st := &fakeEnvelopes{acked: map[string]int64{}}
	rep, err := ImportStream(st, strings.NewReader(envLine(1, EnvelopeSchema)))
	if err == nil || rep.Applied != 0 || len(st.applied) != 0 || st.acked["peer"] != 0 {
		t.Fatalf("nontransactional import = %+v, applied %v (%v)", rep, st.applied, err)
	}
}

func TestPeerAnswersRequireProtocolFields(t *testing.T) {
	for _, bad := range []string{`null`, `{}`, `{"since":null}`, `{"since":{"bad origin":1}}`, `{"since":{"a":-1}}`} {
		if err := decodeSince([]byte(bad), new(serveSince)); err == nil {
			t.Errorf("watermark %q accepted", bad)
		}
	}
	for _, bad := range []string{`null`, `{}`, `{"applied":0}`, `{"applied":null,"dup":0,"conflicts":0}`, `{"applied":0,"dup":null,"conflicts":0}`,
		`{"applied":0,"dup":0,"conflicts":null}`, `{"applied":-1,"dup":0,"conflicts":0}`} {
		if err := decodeApplied([]byte(bad), new(serveApplied)); err == nil {
			t.Errorf("summary %q accepted", bad)
		}
	}
}

func TestPushRetriesCappedStreamUntilEmpty(t *testing.T) {
	st := fakeRows("peer", 2)
	payload := json.RawMessage(`"` + strings.Repeat("x", 5<<20) + `"`)
	for i := range st.rows["peer"] {
		st.rows["peer"][i].Payload = payload
	}
	var peerSeq int64
	calls := 0
	defer StubSyncSSH(func(_ context.Context, _, _ string, in io.Reader, out io.Writer) (string, int, error) {
		calls++
		fmt.Fprintf(out, `{"since":{"peer":%d}}`+"\n", peerSeq)
		body, err := io.ReadAll(in)
		if err != nil {
			return "", 1, err
		}
		_, n, err := ReadStream(bytes.NewReader(body))
		if err != nil {
			return "", 1, err
		}
		peerSeq += int64(n)
		fmt.Fprintf(out, `{"applied":%d,"dup":0,"conflicts":0}`+"\n", n)
		return "", 0, nil
	})()
	var out strings.Builder
	if got := pushPeer(context.Background(), st, "peer.example", &out); got != syncOK || peerSeq != 2 || calls != 3 {
		t.Fatalf("capped push = %d, peer seq %d, calls %d: %s", got, peerSeq, calls, out.String())
	}
}

func TestPushRefusesHeldPeerReply(t *testing.T) {
	st := fakeRows("peer", 1)
	defer StubSyncSSH(func(_ context.Context, _, _ string, in io.Reader, out io.Writer) (string, int, error) {
		fmt.Fprintln(out, `{"since":{}}`)
		if _, err := io.Copy(io.Discard, in); err != nil {
			return "", 1, err
		}
		fmt.Fprintln(out, `{"applied":0,"dup":0,"conflicts":0,"held":["schema mismatch"]}`)
		return "", 0, nil
	})()
	if got := pushPeer(context.Background(), st, "peer.example", io.Discard); got != syncHeld {
		t.Fatalf("held peer reply = %d, want held", got)
	}
}

func TestOldPeerExitDoesNotStartPush(t *testing.T) {
	st := fakeRows("local", 1)
	calls := 0
	defer StubSyncSSH(func(_ context.Context, _, _ string, _ io.Reader, _ io.Writer) (string, int, error) {
		calls++
		return "unknown memory sync --serve", 2, nil
	})()
	var out strings.Builder
	if got := SyncPeers(context.Background(), st, []string{"old.example"}, &out); got != 1 || calls != 1 || len(st.applied) != 0 {
		t.Fatalf("old peer: exit %d, calls %d, applied %v: %s", got, calls, st.applied, out.String())
	}
}

func TestPushSSHReleasesReaderWhenPeerExitsEarly(t *testing.T) {
	if os.Getenv("MEMORY_SYNC_EXIT_EARLY") == "1" {
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPushSSHReleasesReaderWhenPeerExitsEarly$")
	cmd.Env = append(os.Environ(), "MEMORY_SYNC_EXIT_EARLY=1")
	body := &lazyBody{ctx: ctx, ready: make(chan struct{}), stopped: make(chan struct{})}
	start := time.Now()
	err := runPushSSH(cmd, body)
	if err == nil || time.Since(start) >= time.Second || ctx.Err() != nil {
		t.Fatalf("early peer exit took %s: %v", time.Since(start), err)
	}
}

func syncFixture(t *testing.T, on string, peers ...string) *Spawns {
	t.Helper()
	tempConfig(t)
	t.Setenv("NETWORK_MEMORY", on)
	t.Setenv("CLAUDE_REMOTE_HOST", "")
	prev := remoteHosts
	remoteHosts = func() []string { return peers }
	t.Cleanup(func() { remoteHosts = prev })
	return StubSpawn(t, true)
}

func syncStamp() string { return filepath.Join(paths.StateDir(), "preflight", syncStampName) }

func TestSyncOffByDefault(t *testing.T) {
	spawns := syncFixture(t, "", "peer.example")
	now := time.Now()
	for _, setup := range []func(){func() {}, func() { t.Setenv("CLAUDE_REMOTE_HOST", "peer.example") }, func() { t.Setenv("NETWORK_MEMORY", "off") }} {
		setup()
		if SyncIfDue(now, now, t.TempDir()) {
			t.Fatal("SyncIfDue started a pass without the opt-in")
		}
	}
	if spawns.Sync.Load() != 0 || pathExists(syncStamp()) {
		t.Fatalf("off left traces: spawns=%d", spawns.Sync.Load())
	}
	t.Setenv("NETWORK_MEMORY", "ON")
	if !SyncOn() {
		t.Fatal("NETWORK_MEMORY=ON is not an opt-in")
	}
}

func TestSyncIfDue(t *testing.T) {
	spawns := syncFixture(t, "on", "peer.example")
	now := time.Now()
	write(t, syncStamp(), "")
	stampAt := func(at time.Time) {
		if err := os.Chtimes(syncStamp(), at, at); err != nil {
			t.Fatal(err)
		}
	}
	stampAt(now.Add(-syncEvery / 2))
	if SyncIfDue(now, now, "") {
		t.Fatal("a fresh stamp started a pass")
	}
	stampAt(now.Add(-syncEvery - time.Hour))
	if SyncIfDue(now, now.Add(-syncEvery-2*time.Hour), "") {
		t.Fatal("a due slot without activity started a pass")
	}
	if !SyncIfDue(now, now, "") || SyncIfDue(now, now, "") || spawns.Sync.Load() != 1 {
		t.Fatalf("due and active: want exactly one pass, got %d", spawns.Sync.Load())
	}
	spawns = syncFixture(t, "on")
	if SyncIfDue(now, now, "") || spawns.Sync.Load() != 0 {
		t.Fatal("an empty peer list started a pass")
	}
}

func TestSyncRefusesInSandboxButNotInNormalDir(t *testing.T) {
	spawns := syncFixture(t, "on", "peer.example")
	now := time.Now()
	if !SyncOn() || !SyncIfDue(now, now, t.TempDir()) || spawns.Sync.Load() != 1 {
		t.Fatalf("a normal config dir did not reach the transport: spawns=%d", spawns.Sync.Load())
	}
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if err := recordSandbox(cfg); err != nil {
		t.Fatal(err)
	}
	if SyncOn() || SyncIfDue(now.Add(syncEvery*2), now.Add(syncEvery*2), t.TempDir()) || spawns.Sync.Load() != 1 {
		t.Fatalf("a sandbox config dir synced: spawns=%d", spawns.Sync.Load())
	}
}

func TestSyncRefusesOptionShapedPeer(t *testing.T) {
	syncFixture(t, "on")
	called := false
	defer StubSyncSSH(func(context.Context, string, string, io.Reader, io.Writer) (string, int, error) {
		called = true
		return "", 0, nil
	})()
	if code, err := RunSync([]string{"-oProxyCommand=x"}, io.Discard); err != nil || code != 2 || called {
		t.Fatalf("code %d, err %v, called %v; want 2 before any ssh", code, err, called)
	}
}

func TestSyncArgvKeepsConnectTimeout(t *testing.T) {
	argv, err := syncArgv("peer.example", "true")
	if err != nil || !slices.Contains(argv, "ConnectTimeout=5") {
		t.Fatalf("argv = %v (%v)", argv, err)
	}
}

func TestPushRetriesDuplicateOnlyReply(t *testing.T) {
	st := fakeRows("peer", 1)
	calls := 0
	defer StubSyncSSH(func(_ context.Context, _, _ string, in io.Reader, out io.Writer) (string, int, error) {
		fmt.Fprintf(out, `{"since":{"peer":%d}}`+"\n", calls)
		calls++
		if _, err := io.Copy(io.Discard, in); err != nil {
			return "", 1, err
		}
		dup := 0
		if calls == 1 {
			dup = 1
		}
		fmt.Fprintf(out, `{"applied":0,"dup":%d,"conflicts":0}`+"\n", dup)
		return "", 0, nil
	})()
	if got := pushPeer(context.Background(), st, "peer.example", io.Discard); got != syncOK || calls != 2 {
		t.Fatalf("duplicate-only push: result %d, calls %d", got, calls)
	}
}

func TestPushReportsLocalExportError(t *testing.T) {
	st := fakeRows("peer", 1)
	st.rows["peer"][0].Payload = json.RawMessage(`invalid JSON`)
	defer StubSyncSSH(func(_ context.Context, _, _ string, in io.Reader, out io.Writer) (string, int, error) {
		fmt.Fprintln(out, `{"since":{}}`)
		if _, err := io.Copy(io.Discard, in); err == nil {
			t.Error("invalid payload exported")
		}
		return "peer refused truncated input", 1, nil
	})()
	var out strings.Builder
	if got := pushPeer(context.Background(), st, "peer.example", &out); got != syncRefused || !strings.Contains(out.String(), "local export:") {
		t.Fatalf("local export failure: result %d, out %q", got, out.String())
	}
}
