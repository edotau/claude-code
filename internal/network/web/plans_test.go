package web

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const plansFixtureBody = "# Plan: p\n\n## Tasks\n\n- [x] 1.1 Done owns: a.go (abc1234)\n" +
	"- [ ] 1.2 Open owns: b.go <!-- working: bob -->\n"

func writePlansFile(t *testing.T, path, body string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func plansServer(t *testing.T) (*Server, string) {
	t.Helper()
	withEmptyDispatchDir(t)
	dir := t.TempDir()
	now := time.Now()
	writePlansFile(t, filepath.Join(dir, "plans", "2026-10-01-old.md"), plansFixtureBody, now.Add(-time.Hour))
	writePlansFile(t, filepath.Join(dir, "plans", "widgets", "2026-10-02-new.md"), plansFixtureBody, now)
	writePlansFile(t, filepath.Join(dir, "plans", "notes.md"), "no checklist here\n", now)
	return newTestServer(t, Options{ClaudeDir: dir, Days: 36500}, nil), dir
}

func getPlanList(t *testing.T, s *Server) []planEntry {
	t.Helper()
	w := do(s, newReq(t, "GET", "/api/plans", "127.0.0.1:7777"))
	if w.Code != http.StatusOK {
		t.Fatalf("/api/plans = %d %s", w.Code, w.Body)
	}
	var list []planEntry
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	return list
}

func getPlan(t *testing.T, s *Server, p string) *httptest.ResponseRecorder {
	t.Helper()
	return do(s, newReq(t, "GET", "/api/plan?path="+url.QueryEscape(p), "127.0.0.1:7777"))
}

// /api/plans lists newest first with counts and claims.
func TestPlansAPIList(t *testing.T) {
	s, _ := plansServer(t)
	list := getPlanList(t, s)
	if len(list) != 2 {
		t.Fatalf("plans = %+v, want 2 (checklist-less notes.md excluded)", list)
	}
	if list[0].Path != "widgets/2026-10-02-new.md" || list[0].Repo != "widgets" || list[1].Repo != rootRepo {
		t.Fatalf("order/repo = %+v", list)
	}
	if list[0].Ticked != 1 || list[0].Total != 2 || len(list[0].Claims) != 1 || !strings.Contains(list[0].Claims[0], "bob") {
		t.Fatalf("counts/claims = %+v", list[0])
	}
}

// /api/plan returns the parsed tasks.
func TestPlansAPIDetail(t *testing.T) {
	s, _ := plansServer(t)
	w := getPlan(t, s, "widgets/2026-10-02-new.md")
	var det planDetail
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &det) != nil || len(det.Tasks) != 2 ||
		!det.Tasks[0].Done || det.Tasks[0].Sha != "abc1234" || det.Tasks[1].Claim == "" {
		t.Fatalf("/api/plan = %d %s", w.Code, w.Body)
	}
}

// /api/plan rejects escapes, symlinks out of the plans root and non-.md paths.
func TestPlansAPIPathGuard(t *testing.T) {
	s, dir := plansServer(t)
	outside := t.TempDir()
	writePlansFile(t, filepath.Join(outside, "secret.md"), "secret\n", time.Now())
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(dir, "plans", "link.md")); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../x.md", "widgets/../../x.md", filepath.Join(outside, "secret.md"), "widgets/2026-10-02-new.txt", "link.md", ""} {
		if w := getPlan(t, s, bad); w.Code != http.StatusBadRequest {
			t.Errorf("path %q = %d, want 400", bad, w.Code)
		}
	}
}

type sseFrame struct{ ev, data string }

// plansStream is a live /api/events connection delivering every frame, with the hubs running.
type plansStream struct{ frames chan sseFrame }

// startPlansStream runs the hub loops with a fast scan interval and opens /api/events.
func startPlansStream(t *testing.T, s *Server) *plansStream {
	t.Helper()
	orig := scanInterval
	scanInterval = 20 * time.Millisecond
	t.Cleanup(func() { scanInterval = orig })

	ts := httptest.NewServer(s.mux)
	ctx, cancel := context.WithCancel(context.Background())
	hubDone, plansDone := make(chan struct{}), make(chan struct{})
	go func() { defer close(hubDone); s.runHub(ctx) }()
	go func() { defer close(plansDone); s.plansHub().run(ctx) }()
	t.Cleanup(func() { ts.CloseClientConnections(); ts.Close(); cancel(); <-hubDone; <-plansDone })

	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/events", nil)
	req.Host = "127.0.0.1:7777"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	frames := make(chan sseFrame, 32)
	go scanSSEFrames(ctx, resp.Body, frames)
	return &plansStream{frames: frames}
}

// scanSSEFrames forwards every event/data pair of an SSE body to frames, closing it at EOF.
func scanSSEFrames(ctx context.Context, body io.Reader, frames chan<- sseFrame) {
	defer close(frames)
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	ev := ""
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "event: "); ok {
			ev = v
		} else if v, ok := strings.CutPrefix(line, "data: "); ok {
			select {
			case frames <- sseFrame{ev, v}:
			case <-ctx.Done():
				return
			}
		}
	}
}

func (ps *plansStream) next(d time.Duration) (sseFrame, bool) {
	select {
	case f, ok := <-ps.frames:
		return f, ok
	case <-time.After(d):
		return sseFrame{}, false
	}
}

// tickedOld is the ticked count of the old plan in a plans frame payload (-1 on bad JSON).
func tickedOld(data string) int {
	var l []planEntry
	if json.Unmarshal([]byte(data), &l) != nil {
		return -1
	}
	n := 0
	for _, e := range l {
		if e.Path == "2026-10-01-old.md" {
			n = e.Ticked
		}
	}
	return n
}

// awaitInitialPlans reads up to two frames for the first plans frame and reports whether a tree frame came first.
func (ps *plansStream) awaitInitialPlans(t *testing.T) (first sseFrame, sawTree bool) {
	t.Helper()
	for i := 0; i < 2 && first.ev != "plans"; i++ {
		f, ok := ps.next(2 * time.Second)
		if !ok {
			t.Fatal("no initial frames")
		}
		if f.ev == "tree" {
			sawTree = true
		} else {
			first = f
		}
	}
	return first, sawTree
}

// awaitTicked waits for a plans frame whose old-plan tick count equals want, and requires a tree frame by then.
func (ps *plansStream) awaitTicked(t *testing.T, want int, sawTree bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case f, ok := <-ps.frames:
			if !ok {
				t.Fatal("stream closed")
			}
			if f.ev == "tree" {
				sawTree = true
			}
			if f.ev == "plans" && tickedOld(f.data) == want {
				if !sawTree {
					t.Fatal("no tree frame arrived alongside plans")
				}
				return
			}
		case <-deadline:
			t.Fatal("no plans frame with the updated tick count within 2s")
		}
	}
}

// SSE: the initial plans frame, then a new frame once a plan is rewritten to ticked, with tree frames intact.
func TestEventsStreamsPlansChange(t *testing.T) {
	s, dir := plansServer(t)
	ps := startPlansStream(t, s)
	first, sawTree := ps.awaitInitialPlans(t)
	if first.ev != "plans" || tickedOld(first.data) != 1 {
		t.Fatalf("initial plans frame = %+v", first)
	}
	p := filepath.Join(dir, "plans", "2026-10-01-old.md")
	body := strings.Replace(plansFixtureBody, "- [ ] 1.2 Open", "- [x] 1.2 Open", 1)
	writePlansFile(t, p, body, time.Now().Add(time.Minute))
	ps.awaitTicked(t, 2, sawTree)
}
