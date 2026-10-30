package remote

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// paneRecordStart/paneRecordEnd frame one pane's capture in captureCommand's output; SOH/STX are
// control bytes vanishingly unlikely to appear in rendered terminal text, so splitting is exact.
const (
	paneRecordStart = "\x01"
	paneRecordEnd   = "\x02"
)

const (
	paneCaptureLines = 50               // trailing lines tmux captures per pane
	maxCaptureBytes  = 1 << 20          // bounds one host's whole capture blob
	maxPanes         = 64               // bounds how many panes one call parses
	captureTimeout   = 20 * time.Second // bounds one host's whole list+capture round trip
)

// captureCommand is the ssh-side script: list cc-* panes, then capture-pane -p -S -N each one,
// framed by control bytes so parseCaptureBlob can split the output back into per-pane records. It
// reuses dispatch.go's fieldSep ("|") for the header columns: tmux's format engine replaces a real
// tab byte with "_" in its own -F output (see fieldSep's comment), so tab cannot separate these.
//
// list-panes's own exit status is checked explicitly, outside any pipe, before filtering/looping:
// piped straight into awk/while, a list-panes failure (exit nonzero, empty stdout) is indistinguishable
// from "no cc-* panes exist" (both yield zero loop iterations, and a while loop that never ran its
// body exits 0) — the ssh round trip reports success with an empty capture, and CapturePanes/Observe
// then has no way to tell a transient tmux failure apart from every tracked pane having vanished. A
// capture-pane failure inside the loop instead `exit`s the loop (its subshell, piped as the script's
// last statement) immediately, which is still the pipeline's — and so the whole script's — exit status.
var captureCommand = `list_out="$(tmux list-panes -a -F '#{session_name}` + fieldSep + `#{pane_id}` + fieldSep +
	`#{@cc_agent}` + fieldSep + `#{pane_current_command}' 2>/dev/null)"; list_rc=$?` +
	`; if [ "$list_rc" -ne 0 ]; then echo 'dispatch: tmux list-panes failed' >&2; exit 1; fi` +
	`; printf '%s\n' "$list_out"` +
	` | awk -F'` + fieldSep + `' '$1 ~ /^cc-/'` +
	` | while IFS='` + fieldSep + `' read -r sess pid agent cmd; do` +
	` printf '` + paneRecordStart + `%s` + fieldSep + `%s` + fieldSep + `%s` + fieldSep + `%s\n' "$sess" "$pid" "$agent" "$cmd";` +
	` if ! tmux capture-pane -p -t "$pid" -S -` + strconv.Itoa(paneCaptureLines) + ` 2>/dev/null; then exit 1; fi;` +
	` printf '` + paneRecordEnd + `\n'; done`

// PaneCapture is one pane's raw sample: tmux-reported identity plus its last captured lines.
type PaneCapture struct {
	Host, Session, PaneID, Agent, CurrentCmd string
	Text                                     string
}

// Runner executes argv (as built by ExecArgs) and returns its output; production code wraps
// exec.CommandContext, tests substitute a fake with no real ssh/tmux involved.
type Runner func(ctx context.Context, argv []string) ([]byte, error)

// ExecRunner is the production Runner.
func ExecRunner(ctx context.Context, argv []string) ([]byte, error) {
	return exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
}

// CapturePanes lists and captures every cc-* tmux pane on host via run, bounding the round trip to
// captureTimeout and the parsed result to maxCaptureBytes/maxPanes; batch forbids ssh password prompts.
func CapturePanes(ctx context.Context, host string, batch bool, run Runner) ([]PaneCapture, error) {
	cctx, cancel := context.WithTimeout(ctx, captureTimeout)
	defer cancel()
	argv := ExecArgs(cctx, host, captureCommand, batch)
	out, err := run(cctx, argv)
	if err != nil {
		return nil, fmt.Errorf("dispatch: capture panes on %s: %w", host, err)
	}
	if len(out) > maxCaptureBytes {
		out = out[:maxCaptureBytes]
	}
	return parseCaptureBlob(host, string(out)), nil
}

// parseCaptureBlob splits captureCommand's framed output into PaneCapture records, best-effort: a
// malformed or truncated record is skipped rather than misparsed.
func parseCaptureBlob(host, blob string) []PaneCapture {
	parts := strings.Split(blob, paneRecordStart)
	caps := make([]PaneCapture, 0, len(parts))
	for _, rec := range parts {
		if rec == "" {
			continue
		}
		if len(caps) >= maxPanes {
			break
		}
		header, text, ok := strings.Cut(rec, "\n")
		if !ok {
			continue
		}
		text, _, _ = strings.Cut(text, paneRecordEnd)
		cols := strings.Split(header, fieldSep)
		if len(cols) < 4 {
			continue
		}
		caps = append(caps, PaneCapture{
			Host: host, Session: cols[0], PaneID: cols[1], Agent: cols[2], CurrentCmd: cols[3],
			Text: strings.TrimRight(text, "\n"),
		})
	}
	return caps
}
