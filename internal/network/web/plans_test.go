package web

import (
	"bufio"
	"context"
	"encoding/json"
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

// Spanning: /api/plans lists newest first with counts and claims; /api/plan returns tasks and rejects escapes.
func TestPlansAPIListDetailAndPathGuard(t *testing.T) {
	s, dir := plansServer(t)
	w := do(s, newReq(t, "GET", "/api/plans", "127.0.0.1:7777"))
	if w.Code != http.StatusOK {
		t.Fatalf("/api/plans = %d %s", w.Code, w.Body)
	}
	var list []planEntry
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("plans = %+v, want 2 (checklist-less notes.md excluded)", list)
	}
	if list[0].Path != "widgets/2026-10-02-new.md" || list[0].Repo != "widgets" || list[1].Repo != rootRepo {
		t.Fatalf("order/repo = %+v", list)
	}
	if list[0].Ticked != 1 || list[0].Total != 2 || len(list[0].Claims) != 1 || !strings.Contains(list[0].Claims[0], "bob") {
		t.Fatalf("counts/claims = %+v", list[0])
	}

	get := func(p string) *httptest.ResponseRecorder {
		return do(s, newReq(t, "GET", "/api/plan?path="+url.QueryEscape(p), "127.0.0.1:7777"))
	}
	w = get("widgets/2026-10-02-new.md")
	var det planDetail
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &det) != nil || len(det.Tasks) != 2 ||
		!det.Tasks[0].Done || det.Tasks[0].Sha != "abc1234" || det.Tasks[1].Claim == "" {
		t.Fatalf("/api/plan = %d %s", w.Code, w.Body)
	}

	outside := t.TempDir()
	writePlansFile(t, filepath.Join(outside, "secret.md"), "secret\n", time.Now())
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(dir, "plans", "link.md")); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../x.md", "widgets/../../x.md", filepath.Join(outside, "secret.md"), "widgets/2026-10-02-new.txt", "link.md", ""} {
		if w := get(bad); w.Code != http.StatusBadRequest {
			t.Errorf("path %q = %d, want 400", bad, w.Code)
		}
	}
}

// SSE: the initial plans frame, then a new frame once a plan is rewritten to ticked, with tree frames intact.
func TestEventsStreamsPlansChange(t *testing.T) {
	s, dir := plansServer(t)
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
	defer resp.Body.Close()
	type frame struct{ ev, data string }
	frames := make(chan frame, 32)
	go func() {
		defer close(frames)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		ev := ""
		for sc.Scan() {
			line := sc.Text()
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				ev = v
			} else if v, ok := strings.CutPrefix(line, "data: "); ok {
				select {
				case frames <- frame{ev, v}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	next := func(d time.Duration) (frame, bool) {
		select {
		case f, ok := <-frames:
			return f, ok
		case <-time.After(d):
			return frame{}, false
		}
	}
	ticked := func(data string) int {
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

	sawTree := false
	var first frame
	for i := 0; i < 2 && first.ev != "plans"; i++ {
		f, ok := next(2 * time.Second)
		if !ok {
			t.Fatal("no initial frames")
		}
		if f.ev == "tree" {
			sawTree = true
		} else {
			first = f
		}
	}
	if first.ev != "plans" || ticked(first.data) != 1 {
		t.Fatalf("initial plans frame = %+v", first)
	}

	p := filepath.Join(dir, "plans", "2026-10-01-old.md")
	body := strings.Replace(plansFixtureBody, "- [ ] 1.2 Open", "- [x] 1.2 Open", 1)
	writePlansFile(t, p, body, time.Now().Add(time.Minute))
	deadline := time.After(2 * time.Second)
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatal("stream closed")
			}
			if f.ev == "tree" {
				sawTree = true
			}
			if f.ev == "plans" && ticked(f.data) == 2 {
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
