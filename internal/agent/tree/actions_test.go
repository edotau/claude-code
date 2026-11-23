package tree

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/textutil"
)

func actionNode(kind Kind) Node {
	return Node{Kind: kind, ID: "job-alpha", Agent: "claude",
		Dispatch: &Dispatch{Agent: "claude", Status: statusRunning}}
}

func stubActionCancel(t *testing.T, fn func(string) (string, error)) {
	t.Helper()
	previous := cancelJob
	cancelJob = fn
	t.Cleanup(func() { cancelJob = previous })
}

func TestOnActionKinds(t *testing.T) {
	cancelLine := "cancel claude job-alpha? Y confirms, any other key keeps it"
	actions := []struct {
		name    string
		a       action
		pending string
		allowed effect
		refused string
	}{
		{"cancel", actCancel, "job-alpha", effect{Kind: effectShow, Line: cancelLine}, "c: only a running job cancels"},
		{"copy", actCopy, "", effect{Kind: effectCopy, ID: "job-alpha", Line: "copied job-alpha"},
			"y: no job id on this row"},
		{"result", actResult, "", effect{Kind: effectShow, Line: "claude-code agents result -f job-alpha"},
			"r: no job id on this row"},
	}
	for _, kind := range []Kind{KindSession, KindSubagent, KindWorkflow, KindDispatch} {
		for _, tt := range actions {
			t.Run(string(kind)+"/"+tt.name, func(t *testing.T) {
				pending, got := onAction("", tt.a, actionNode(kind))
				want := effect{Kind: effectShow, Line: tt.refused}
				wantPending := ""
				if kind == KindDispatch {
					wantPending, want = tt.pending, tt.allowed
				}
				if pending != wantPending || got != want {
					t.Fatalf("onAction = (%q, %+v), want (%q, %+v)", pending, got, wantPending, want)
				}
			})
		}
	}
}

func TestActionCancelConfirm(t *testing.T) {
	for _, confirmed := range []bool{true, false} {
		calls := 0
		stubActionCancel(t, func(id string) (string, error) {
			calls++
			return "cancelled " + id, nil
		})
		n := actionNode(KindDispatch)
		pending, e := onAction("", actCancel, n)
		perform(e, io.Discard)
		if calls != 0 || pending != n.ID {
			t.Fatalf("first press: calls=%d pending=%q", calls, pending)
		}
		pending, e = onConfirm(pending, confirmed, n)
		line := perform(e, io.Discard)
		wantCalls, wantLine := 0, "cancel kept"
		if confirmed {
			wantCalls, wantLine = 1, "cancelled job-alpha"
		}
		if pending != "" || calls != wantCalls || line != wantLine {
			t.Fatalf("confirmed=%v: pending=%q calls=%d line=%q", confirmed, pending, calls, line)
		}
		if _, e = onConfirm(pending, true, n); perform(e, io.Discard) != "" || calls != wantCalls {
			t.Fatal("a confirm with nothing armed cancelled")
		}
	}
}

func TestActionCancelRefused(t *testing.T) {
	finished := actionNode(KindDispatch)
	finished.Dispatch.Status = statusFinished
	for _, n := range []Node{finished, {Kind: KindDispatch, ID: "missing"}} {
		pending, e := onAction("", actCancel, n)
		if pending != "" || e.Kind != effectShow || e.Line != "c: only a running job cancels" {
			t.Fatalf("cancel refused = (%q, %+v)", pending, e)
		}
	}
}

func TestActionConfirmChangedRow(t *testing.T) {
	changed := actionNode(KindDispatch)
	changed.ID = "another-job"
	finished := actionNode(KindDispatch)
	finished.Dispatch.Status = statusFinished
	for _, n := range []Node{changed, finished, actionNode(KindSession), actionNode(KindSubagent)} {
		pending, e := onConfirm("job-alpha", true, n)
		if pending != "" || e != (effect{Kind: effectShow, Line: "cancel kept"}) {
			t.Fatalf("changed selection = (%q, %+v)", pending, e)
		}
	}
}

func TestActionCarrier(t *testing.T) {
	n := actionNode(KindDispatch)
	n.Via = "carrier-beta"
	_, copied := onAction("", actCopy, n)
	var out bytes.Buffer
	if got := perform(copied, &out); got != "copied carrier-beta" {
		t.Fatalf("copy status = %q", got)
	}
	if got := out.String(); got != "\x1b]52;c;Y2Fycmllci1iZXRh\a" {
		t.Fatalf("OSC 52 = %q", got)
	}
	if _, result := onAction("", actResult, n); result.Line != "claude-code agents result -f carrier-beta" {
		t.Fatalf("result = %+v", result)
	}
	stubActionCancel(t, func(id string) (string, error) {
		if id != n.Via {
			t.Fatalf("cancel target = %q, want carrier", id)
		}
		return "cancelled " + id, nil
	})
	pending, _ := onAction("", actCancel, n)
	if _, e := onConfirm(pending, true, n); perform(e, io.Discard) != "cancelled carrier-beta" {
		t.Fatalf("carrier cancel = %+v", e)
	}
}

func TestActionLineWidth(t *testing.T) {
	n := actionNode(KindDispatch)
	n.Agent, n.ID = strings.Repeat("界", maxLineWidth+1), strings.Repeat("x", maxLineWidth+1)
	for _, a := range []action{actCancel, actCopy, actResult} {
		_, e := onAction("", a, n)
		if width := textutil.DisplayWidth(e.Line); width > maxLineWidth {
			t.Fatalf("status width = %d: %q", width, e.Line)
		}
		if a == actCancel && !strings.HasSuffix(e.Line, "? Y confirms, any other key keeps it") {
			t.Fatalf("confirmation instruction clipped: %q", e.Line)
		}
	}
	stubActionCancel(t, func(string) (string, error) { return "", errors.New(strings.Repeat("界", maxLineWidth+1)) })
	if width := textutil.DisplayWidth(perform(effect{Kind: effectCancel, ID: n.ID}, io.Discard)); width > maxLineWidth {
		t.Fatalf("error width = %d", width)
	}
}
