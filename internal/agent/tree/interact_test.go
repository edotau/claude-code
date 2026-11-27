package tree

import (
	"bytes"
	"context"
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
)

func TestDecodeKeys(t *testing.T) {
	got := decodeKeys([]byte("\x1b[A\x1bOBj\rq\x1bx\x1b[3~cY"))
	want := []key{keyUp, keyDown, keyDown, keyEnter, keyQuit, keyEsc, keyOther, keyOther, keyOther, keyCancel, keyConfirm}
	if !slices.Equal(got, want) {
		t.Errorf("decodeKeys = %v, want %v", got, want)
	}
}

// c arms the cancel through the real key handler; a key other than Y disarms it and nothing is cancelled.
func TestDashboardCancelNeedsCapitalY(t *testing.T) {
	stubActionCancel(t, func(id string) (string, error) {
		t.Fatalf("cancelled %s without Y", id)
		return "", nil
	})
	n := actionNode(KindDispatch)
	d := &dashboard{out: io.Discard, nodes: []Node{n, {Kind: KindDispatch, ID: "other"}}, st: dashState{height: 24}}
	for _, k := range decodeKeys([]byte("n\x1b[B")) { // an unbound key, then a move
		if d.onKey(keyCancel); d.st.pending != n.ID {
			t.Fatalf("after c: pending %q, want %q", d.st.pending, n.ID)
		}
		if d.onKey(k); d.st.pending != "" || d.st.status != "cancel kept" {
			t.Fatalf("after c, key %d: %+v, want disarmed", k, d.st)
		}
	}
	if d.st.selID != "dispatch:"+n.ID {
		t.Fatalf("the disarming key moved the selection to %q", d.st.selID)
	}
}

// o on a dispatch row loads its record and draws the pane: header, scrubbed task and the live footer.
func TestDashboardOpenKeyOpensPane(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := agent.NewRecord("codex", agent.RunOptions{WorkDir: "/repo"}, time.Now())
	rec.Request.Prompt = "\x1b[31mread the files\x1b[0m"
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	orig, streamed := paneStream, ""
	t.Cleanup(func() { paneStream = orig })
	paneStream = func(ctx context.Context, r agent.Record, _ io.Writer) error { // a live job: streams until the pane closes
		streamed = r.ID
		<-ctx.Done()
		return nil
	}
	keys := make(chan key)
	close(keys)
	var out bytes.Buffer
	d := &dashboard{ctx: t.Context(), out: &out, keys: keys, tree: &Tree{},
		nodes: []Node{{Kind: KindDispatch, ID: rec.ID, Dispatch: &Dispatch{Agent: "codex"}}}}
	d.onKey(keyOpen)
	frame := out.String()
	if streamed != rec.ID || !strings.Contains(frame, "id     "+rec.ID) || !strings.Contains(frame, "live ·") ||
		!strings.Contains(frame, "read the files") || strings.Contains(frame, "[31m") {
		t.Fatalf("streamed %q, frame:\n%q", streamed, frame)
	}
}

func TestDashboardHeaderRefreshStops(t *testing.T) {
	original := headerSource
	var calls atomic.Int32
	headerSource = func(context.Context, string) headerStatus {
		calls.Add(1)
		return headerStatus{}
	}
	t.Cleanup(func() { headerSource = original })
	ctx, cancel := context.WithCancel(t.Context())
	updates := make(chan headerStatus, 1)
	done := make(chan struct{})
	top := t.TempDir()
	go func() { refreshHeader(ctx, top, updates); close(done) }()
	<-updates
	cancel()
	<-done
	if got := calls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
}

func TestTreeLStillMovesRight(t *testing.T) {
	nodes := dashNodes()
	s := clamp(dashState{height: 24}, visible(nodes, nil))
	s = press(s, nodes, keyDown, decodeKeys([]byte("l"))[0])
	if want := press(clamp(dashState{height: 24}, visible(nodes, nil)), nodes, keyDown, keyRight); s.selID != want.selID ||
		len(visible(nodes, s.open)) != len(visible(nodes, want.open)) || len(visible(nodes, s.open)) < 5 {
		t.Errorf("l left %q with %d rows, want the same as the right arrow", s.selID, len(visible(nodes, s.open)))
	}
}

// Enter expands a dispatch row with its task and identity lines, indented under it, and Enter again collapses it.
func TestEnterTogglesInlineExpansion(t *testing.T) {
	d := Dispatch{ID: "1-pi", Agent: "pi", Model: "gpt-6", Status: statusRunning, Task: "review the diff", Provider: "databricks", launch: "qa"}
	nodes := []Node{{Kind: KindDispatch, ID: d.ID, Dispatch: &d}}
	s := press(clamp(dashState{width: 80, height: 24}, visible(nodes, nil)), nodes, keyEnter)
	lines := dashLines(s, nodes, time.Now(), "")
	if len(lines) < 4 || !strings.HasPrefix(strings.TrimLeft(lines[1], " "), "└─ review the diff") ||
		!strings.Contains(lines[2], "running · databricks·qa · gpt-6 · id 1-pi") {
		t.Fatalf("expanded frame:\n%s", strings.Join(lines, "\n"))
	}
	nodes[0].ID = "2-worker" // a carrier's node id changes when its worker takes over; the expansion stays
	if got := dashLines(s, nodes, time.Now(), ""); !strings.Contains(strings.Join(got, "\n"), "└─ review the diff") {
		t.Errorf("expansion lost after the hand-off:\n%s", strings.Join(got, "\n"))
	}
	s = press(s, nodes, keyEnter)
	if got := dashLines(s, nodes, time.Now(), ""); strings.Contains(strings.Join(got, "\n"), "└─") {
		t.Errorf("collapsed frame still shows the task:\n%s", strings.Join(got, "\n"))
	}
}
