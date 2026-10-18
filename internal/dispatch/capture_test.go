package dispatch

import (
	"context"
	"errors"
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
	for i := 0; i < maxPanes+10; i++ {
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
