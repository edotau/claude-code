package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agenttree"
)

// hubFixture is a Server over a temp copy of the agenttree fixture with fast scan timings and a counted Build.
type hubFixture struct {
	s      *Server
	ts     *httptest.Server
	dir    string
	builds *atomic.Int64
	stop   func()
}

func newHubFixture(t *testing.T) *hubFixture {
	t.Helper()
	withEmptyDispatchDir(t)
	dir := t.TempDir()
	if err := copyDir(t, agenttreeTestdata, dir); err != nil {
		t.Fatal(err)
	}
	var builds atomic.Int64
	origBuild, origScan := buildTree, scanInterval
	buildTree = func(root string, o agenttree.Options) (*agenttree.Tree, error) {
		builds.Add(1)
		return origBuild(root, o)
	}
	scanInterval = 20 * time.Millisecond
	t.Cleanup(func() { buildTree, scanInterval = origBuild, origScan }) // runs after the hub goroutine exits

	s := newTestServer(t, Options{ClaudeDir: dir, Days: 36500}, nil)
	ts := httptest.NewUnstartedServer(s.mux)
	ts.Start()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.runHub(ctx) }()
	stop := func() { cancel(); <-done }
	t.Cleanup(func() { ts.CloseClientConnections(); ts.Close(); stop() })
	return &hubFixture{s: s, ts: ts, dir: dir, builds: &builds, stop: stop}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", d, what)
}

// openEvents opens /api/events and returns a channel of `event: tree` data payloads plus the cancel func.
func openEvents(t *testing.T, base string) (<-chan string, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "127.0.0.1:7777"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/events: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	frames := make(chan string, 16)
	go func() {
		defer resp.Body.Close()
		defer close(frames)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		ev := ""
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				ev = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: ") && ev == "tree":
				select {
				case frames <- strings.TrimPrefix(line, "data: "):
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return frames, cancel
}

func nextFrame(t *testing.T, frames <-chan string, d time.Duration) string {
	t.Helper()
	select {
	case f, ok := <-frames:
		if !ok {
			t.Fatal("event stream closed before the next tree frame")
		}
		return f
	case <-time.After(d):
		t.Fatalf("no tree frame within %v", d)
	}
	return ""
}

// Spanning test: scanner -> hub -> /api/events frames, first the snapshot, then a frame after a file change.
func TestEventsStreamsTreeThenChange(t *testing.T) {
	f := newHubFixture(t)
	frames, _ := openEvents(t, f.ts.URL)

	first := nextFrame(t, frames, 5*time.Second)
	var tree agenttree.Tree
	if err := json.Unmarshal([]byte(first), &tree); err != nil {
		t.Fatalf("first frame is not agenttree JSON: %v; %s", err, first)
	}
	if len(tree.Sessions) == 0 {
		t.Fatalf("first frame has no sessions: %s", first)
	}

	transcript := filepath.Join(f.dir, "projects", "-Users-fake-demo-repo", "running-session-bbbb2222.jsonl")
	fh, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	line := `{"type":"assistant","timestamp":"2026-10-18T03:00:00.000Z","sessionId":"running-session-bbbb2222",` +
		`"message":{"role":"assistant","model":"claude-x","content":[{"type":"text","text":"sse change"}],` +
		`"usage":{"input_tokens":777,"output_tokens":888}}}` + "\n"
	if _, err := fh.WriteString(line); err != nil {
		t.Fatal(err)
	}
	fh.Close()
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(transcript, future, future); err != nil {
		t.Fatal(err)
	}

	second := nextFrame(t, frames, 3*time.Second)
	if second == first {
		t.Fatal("second frame identical to the first; frames must only be sent on change")
	}
	if err := json.Unmarshal([]byte(second), new(agenttree.Tree)); err != nil {
		t.Fatalf("second frame is not agenttree JSON: %v", err)
	}
}

// Subscribers share the scanner: N streams after the first scan cause no extra builds.
func TestEventsSubscribersShareOneBuild(t *testing.T) {
	f := newHubFixture(t)
	h := f.s.treeHub()
	waitFor(t, 5*time.Second, "first scan", func() bool { return h.latest() != nil })
	time.Sleep(100 * time.Millisecond) // let any in-flight tick settle
	before := f.builds.Load()

	const n = 5
	var chans []<-chan string
	for range n {
		fr, _ := openEvents(t, f.ts.URL)
		chans = append(chans, fr)
	}
	waitFor(t, 3*time.Second, "all subscribers registered", func() bool { return h.subscribers() == n })
	for _, fr := range chans {
		if got := nextFrame(t, fr, 3*time.Second); !strings.Contains(got, `"sessions"`) {
			t.Fatalf("subscriber frame lacks sessions: %.200s", got)
		}
	}
	time.Sleep(150 * time.Millisecond) // several scan ticks with unchanged files
	if got := f.builds.Load() - before; got != 0 {
		t.Fatalf("%d builds for %d subscribers on unchanged files, want 0", got, n)
	}
}

// /api/tree after the first scan serves the hub's bytes and does not build.
func TestTreeServesHubCache(t *testing.T) {
	f := newHubFixture(t)
	h := f.s.treeHub()
	waitFor(t, 5*time.Second, "first scan", func() bool { return h.latest() != nil })
	time.Sleep(100 * time.Millisecond)
	before := f.builds.Load()

	req, _ := http.NewRequest("GET", f.ts.URL+"/api/tree", nil)
	req.Host = "127.0.0.1:7777"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		body.WriteString(sc.Text())
		body.WriteString("\n")
	}
	if body.String() != string(h.latest()) {
		t.Fatalf("/api/tree body differs from hub cache:\n%.300s\nvs\n%.300s", body.String(), h.latest())
	}
	if got := f.builds.Load() - before; got != 0 {
		t.Fatalf("/api/tree triggered %d builds, want 0", got)
	}
}

// A client that goes away is dropped from the hub.
func TestEventsDisconnectUnsubscribes(t *testing.T) {
	f := newHubFixture(t)
	h := f.s.treeHub()
	frames, cancel := openEvents(t, f.ts.URL)
	nextFrame(t, frames, 5*time.Second)
	waitFor(t, 3*time.Second, "subscriber registered", func() bool { return h.subscribers() == 1 })

	cancel()
	waitFor(t, 3*time.Second, "subscriber removal", func() bool { return h.subscribers() == 0 })
}

// run returns on ctx cancel, and an unchanged fingerprint between ticks never rebuilds.
func TestHubRunStopsAndSkipsUnchanged(t *testing.T) {
	origScan := scanInterval
	scanInterval = 5 * time.Millisecond
	defer func() { scanInterval = origScan }()

	var builds, fps atomic.Int64
	h := newHub(
		func() ([]byte, error) { builds.Add(1); return []byte("{}\n"), nil },
		func() uint64 { fps.Add(1); return 42 },
		func(string, ...any) {},
	)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { ; h.run(ctx) })

	waitFor(t, 2*time.Second, "several fingerprint ticks", func() bool { return fps.Load() >= 5 })
	if got := builds.Load(); got != 1 {
		t.Fatalf("builds = %d with a constant fingerprint, want 1 (initial scan only)", got)
	}

	cancel()
	stopped := make(chan struct{})
	go func() { wg.Wait(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not return after ctx cancel")
	}
	select {
	case <-h.done:
	default:
		t.Fatal("h.done not closed after run returned")
	}
}

// Regression: a client that connects and never reads is dropped by the per-frame write deadline.
func TestEventsStalledClientIsDropped(t *testing.T) {
	orig := sseWriteLimit
	sseWriteLimit = 200 * time.Millisecond
	t.Cleanup(func() { sseWriteLimit = orig })
	f := newHubFixture(t)
	h := f.s.treeHub()
	waitFor(t, 5*time.Second, "first scan", func() bool { return h.latest() != nil })
	h.publish([]byte(`{"pad":"` + strings.Repeat("x", 64<<20) + `"}` + "\n")) // far past the socket buffers

	c, err := net.Dial("tcp", f.ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("GET /api/events HTTP/1.1\r\nHost: 127.0.0.1:7777\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "subscriber registered", func() bool { return h.subscribers() == 1 })
	waitFor(t, 5*time.Second, "stalled subscriber dropped", func() bool { return h.subscribers() == 0 })
}

// Regression: a dispatch-record-only change rebuilds without waiting for the heartbeat.
func TestEventsDispatchRecordChangeStreams(t *testing.T) {
	origHB := scanHeartbeat
	scanHeartbeat = time.Hour
	t.Cleanup(func() { scanHeartbeat = origHB })
	f := newHubFixture(t)
	frames, _ := openEvents(t, f.ts.URL)
	first := nextFrame(t, frames, 5*time.Second)

	// finished-session's relay transcript names this id in an "ask: dispatch" line, so its leg comes from the record
	rec := agent.NewRecord("codex", agent.Request{}, time.Date(2026, 10, 17, 23, 11, 31, 948_000_000, time.UTC))
	rec.Status = "ok"
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	if second := nextFrame(t, frames, 3*time.Second); second == first {
		t.Fatal("frame identical after a dispatch record appeared")
	}
}
