package remote

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// fakeRunner returns out/err for every call, regardless of argv, and records the argv it was called with.
func fakeRunner(out []byte, err error) (Runner, *[][]string) {
	var calls [][]string
	return func(_ context.Context, argv []string) ([]byte, error) {
		calls = append(calls, argv)
		return out, err
	}, &calls
}

// frame builds one captureCommand-shaped record: session, pane id, agent, current command, then text.
func frame(sess, pid, agent, cmd, text string) string {
	return paneRecordStart + sess + fieldSep + pid + fieldSep + agent + fieldSep + cmd + "\n" + text + paneRecordEnd + "\n"
}

func TestCapturePanes_ParsesMultipleRecords(t *testing.T) {
	blob := frame("cc-claude-a", "%1", "claude", "claude", "line1\nline2") +
		frame("cc-codex-b", "%2", "codex", "codex", "working...")
	run, _ := fakeRunner([]byte(blob), nil)
	caps, err := CapturePanes(context.Background(), "myhost", true, run)
	if err != nil {
		t.Fatalf("CapturePanes: %v", err)
	}
	if len(caps) != 2 {
		t.Fatalf("got %d captures, want 2: %+v", len(caps), caps)
	}
	if caps[0].Session != "cc-claude-a" || caps[0].PaneID != "%1" || caps[0].Agent != "claude" || caps[0].Text != "line1\nline2" {
		t.Errorf("caps[0] = %+v, unexpected", caps[0])
	}
	if caps[1].Session != "cc-codex-b" || caps[1].Text != "working..." {
		t.Errorf("caps[1] = %+v, unexpected", caps[1])
	}
	for _, c := range caps {
		if c.Host != "myhost" {
			t.Errorf("Host = %q, want %q", c.Host, "myhost")
		}
	}
}

func TestCapturePanes_SSHFailurePropagatesError(t *testing.T) {
	wantErr := errors.New("ssh: connection refused")
	run, _ := fakeRunner(nil, wantErr)
	caps, err := CapturePanes(context.Background(), "deadhost", true, run)
	if err == nil {
		t.Fatalf("CapturePanes: want error, got nil (caps=%+v)", caps)
	}
	if !strings.Contains(err.Error(), "deadhost") || !strings.Contains(err.Error(), wantErr.Error()) {
		t.Errorf("error = %q, want it to name the host and wrap the underlying error", err.Error())
	}
	if caps != nil {
		t.Errorf("caps = %+v, want nil on error", caps)
	}
}

func TestCapturePanes_EmptyOutputIsEmptyNotError(t *testing.T) {
	run, _ := fakeRunner([]byte(""), nil)
	caps, err := CapturePanes(context.Background(), "myhost", true, run)
	if err != nil {
		t.Fatalf("CapturePanes: %v", err)
	}
	if len(caps) != 0 {
		t.Errorf("caps = %+v, want empty", caps)
	}
}

func TestCapturePanes_MalformedRecordIsSkipped(t *testing.T) {
	// Missing the newline that separates the header from the text: parseCaptureBlob must skip it,
	// not panic or misattribute the following well-formed record's text to it.
	blob := paneRecordStart + "cc-claude-a\t%1\tclaude\tclaude" + // no header/body newline
		frame("cc-codex-b", "%2", "codex", "codex", "ok")
	run, _ := fakeRunner([]byte(blob), nil)
	caps, err := CapturePanes(context.Background(), "myhost", true, run)
	if err != nil {
		t.Fatalf("CapturePanes: %v", err)
	}
	for _, c := range caps {
		if c.Session == "cc-claude-a" {
			t.Errorf("malformed record should have been skipped, got %+v", c)
		}
	}
}

func TestCapturePanes_CapsPaneCount(t *testing.T) {
	var blob strings.Builder
	for range maxPanes + 10 {
		blob.WriteString(frame("cc-claude-x", "%1", "claude", "claude", "line"))
	}
	run, _ := fakeRunner([]byte(blob.String()), nil)
	caps, err := CapturePanes(context.Background(), "myhost", true, run)
	if err != nil {
		t.Fatalf("CapturePanes: %v", err)
	}
	if len(caps) != maxPanes {
		t.Errorf("got %d captures, want capped at %d", len(caps), maxPanes)
	}
}

func TestCapturePanes_CapsBytes(t *testing.T) {
	huge := strings.Repeat("x", maxCaptureBytes*2)
	blob := frame("cc-claude-a", "%1", "claude", "claude", huge)
	run, _ := fakeRunner([]byte(blob), nil)
	caps, err := CapturePanes(context.Background(), "myhost", true, run)
	if err != nil {
		t.Fatalf("CapturePanes: %v", err)
	}
	// The whole blob (not just one pane's text) is truncated to maxCaptureBytes before parsing, so
	// the result must never exceed that regardless of how many/few records survive truncation.
	total := 0
	for _, c := range caps {
		total += len(c.Text)
	}
	if total > maxCaptureBytes {
		t.Errorf("total captured text = %d bytes, want <= %d (maxCaptureBytes)", total, maxCaptureBytes)
	}
}

func TestCapturePanes_InjectionShapedTextIsDataNotControl(t *testing.T) {
	// A pane legitimately echoing shell/HTML-looking text must survive as plain Text, not be
	// interpreted, split further, or crash the parser.
	payload := "$(touch /tmp/pwned); <script>alert(1)</script>; '; DROP TABLE x; --"
	blob := frame("cc-claude-a", "%1", "claude", "claude", payload)
	run, _ := fakeRunner([]byte(blob), nil)
	caps, err := CapturePanes(context.Background(), "myhost", true, run)
	if err != nil {
		t.Fatalf("CapturePanes: %v", err)
	}
	if len(caps) != 1 || caps[0].Text != payload {
		t.Fatalf("injection-shaped payload was not preserved verbatim as data: %+v", caps)
	}
}

// writeStubTmuxListPanesFails puts a `tmux` stub on dir that fails `list-panes` (simulating a
// transient tmux failure, e.g. no server at all) but would succeed on anything else.
func writeStubTmuxListPanesFails(t *testing.T, dir string) {
	t.Helper()
	script := "#!/bin/sh\nif [ \"$1\" = list-panes ]; then echo 'simulated tmux failure' >&2; exit 1; fi\nexit 0\n"
	if err := os.WriteFile(dir+"/tmux", []byte(script), 0o755); err != nil {
		t.Fatalf("writeStubTmuxListPanesFails: %v", err)
	}
}

// TestCapturePanes_RealScriptPropagatesListPanesFailure is the regression test for Appendix A
// codex-review finding #11 (internal/network/remote/capture.go:29): it exercises the REAL captureCommand
// shell script (via ExecRunner, run locally through sh -c since host="localhost") against a stub
// tmux whose `list-panes` fails, and requires CapturePanes to surface that as an error rather than
// a successful empty capture — which previously made every tracked pane look "gone".
func TestCapturePanes_RealScriptPropagatesListPanesFailure(t *testing.T) {
	stubDir := t.TempDir()
	writeStubTmuxListPanesFails(t, stubDir)
	t.Setenv("PATH", stubDir+":"+os.Getenv("PATH"))

	caps, err := CapturePanes(context.Background(), "localhost", true, ExecRunner)
	if err == nil {
		t.Fatalf("CapturePanes: want an error when the real script's tmux list-panes fails, got caps=%+v", caps)
	}
	if caps != nil {
		t.Errorf("caps = %+v, want nil on a propagated listing failure", caps)
	}
}

// writeStubTmuxCapturePaneFails lists one cc-* pane successfully but fails capture-pane on it, and
// touches markerPath so the test can confirm capture-pane actually ran rather than list-panes itself
// having failed first (the printf format string here must treat the pane-id field as DATA, via the
// "%s" format plus a separate argument — inlining it into the format string, e.g.
// `printf 'cc-claude-a|%1|...\n'`, makes printf itself fail on the literal "%1" token before
// list-panes's exit code is even checked, which previously made this test pass for the wrong reason).
func writeStubTmuxCapturePaneFails(t *testing.T, dir, markerPath string) {
	t.Helper()
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  list-panes) printf '%s\\n' 'cc-claude-a|%1|claude|claude' ;;\n" +
		"  capture-pane) touch '" + markerPath + "'; echo 'simulated capture failure' >&2; exit 1 ;;\n" +
		"  *) exit 0 ;;\n" +
		"esac\n"
	if err := os.WriteFile(dir+"/tmux", []byte(script), 0o755); err != nil {
		t.Fatalf("writeStubTmuxCapturePaneFails: %v", err)
	}
}

// TestCapturePanes_RealScriptPropagatesCapturePaneFailure covers the loop-body half of the same
// finding: a capture-pane failure for one listed pane must also fail the whole script, not just
// silently omit that pane's record. The marker-file check pins that list-panes actually succeeded and
// capture-pane was reached — without it, a broken stub that fails list-panes first would make this
// test pass vacuously, for the wrong reason (see writeStubTmuxCapturePaneFails).
func TestCapturePanes_RealScriptPropagatesCapturePaneFailure(t *testing.T) {
	stubDir := t.TempDir()
	marker := stubDir + "/capture-pane-invoked"
	writeStubTmuxCapturePaneFails(t, stubDir, marker)
	t.Setenv("PATH", stubDir+":"+os.Getenv("PATH"))

	caps, err := CapturePanes(context.Background(), "localhost", true, ExecRunner)
	if err == nil {
		t.Fatalf("CapturePanes: want an error when capture-pane fails mid-loop, got caps=%+v", caps)
	}
	if caps != nil {
		t.Errorf("caps = %+v, want nil on a propagated capture failure", caps)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("capture-pane was never invoked (list-panes must have failed first): %v", statErr)
	}
}

func TestCapturePanes_PassesBatchAndHostThroughExecArgs(t *testing.T) {
	var gotArgv []string
	run := func(_ context.Context, argv []string) ([]byte, error) {
		gotArgv = argv
		return []byte(""), nil
	}
	if _, err := CapturePanes(context.Background(), "remote-host", true, run); err != nil {
		t.Fatalf("CapturePanes: %v", err)
	}
	if len(gotArgv) == 0 {
		t.Fatal("run was never called")
	}
	joined := strings.Join(gotArgv, " ")
	if !strings.Contains(joined, "remote-host") {
		t.Errorf("argv does not reference host: %v", gotArgv)
	}
	if !strings.Contains(joined, "BatchMode=yes") {
		t.Errorf("batch=true should select BatchMode=yes ssh args: %v", gotArgv)
	}
}
