package remote

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// State values a pane can be classified into. "unknown" covers static ambiguous output: this
// package never reports "idle" — a pane that merely isn't changing is unknown, not confirmed idle.
const (
	StateWorking     = "working"      // output changing, or an explicit working indicator present
	StateInputNeeded = "input-needed" // an explicit prompt (yes/no, composer box) is waiting on the human
	StateReady       = "ready"        // a settled, recognized prompt with unchanged output
	StateUnknown     = "unknown"      // static output with no recognizable prompt; never called idle
	StateGone        = "gone"         // the pane no longer exists in a successful capture
	StateError       = "error"        // the host-wide capture failed (ssh/tmux error)
)

// PaneStatus is one observed tmux pane's classified state. fingerprint is carried internally across
// ObservePanes calls (bounded, never the captured text) and is deliberately not exported/serialized.
type PaneStatus struct {
	Host       string    `json:"host"`
	Session    string    `json:"session"`
	PaneID     string    `json:"paneId"`
	Agent      string    `json:"agent"`
	State      string    `json:"state"`
	Reason     string    `json:"reason"`
	ObservedAt time.Time `json:"observedAt"`
	ChangedAt  time.Time `json:"changedAt"`

	fingerprint string
}

// ansiRE strips cursor/SGR escape sequences and bare carriage returns, in case capture-pane output
// ever includes them (the default -p omits SGR, but -e or a future caller might not).
var ansiRE = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]|\x1b\][^\x07]*\x07|\r`) })

// spinnerRE normalizes per-frame noise that changes every sample without reflecting new output:
// braille spinner glyphs and ticking elapsed-seconds counters like "(12s · esc to interrupt)".
var spinnerRE = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`[\x{2800}-\x{28FF}]|\(\d+s[^)]*\)|\b\d+s\b`) })

// inputPatterns match an explicit request for the human: a yes/no or numbered choice prompt, a
// bordered TUI composer box (bubbletea/ink style box-drawing), or a blocking "press enter" notice.
var inputPatterns = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?mi)\(y/n\)|\[y/n\]|do you want to proceed|press enter to continue|waiting for (your )?input|^\s*1\.\s*yes|[╭╮╰╯]|│\s*>`)
})

// workingPatterns match text an agent CLI prints while actively generating or running a tool.
var workingPatterns = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?i)esc to interrupt|thinking…|thinking\.\.\.|generating|tokens/s|running tool|ctrl-c to stop`)
})

// readyPatterns match a settled, idle prompt: the agent's bare composer caret or a plain shell prompt.
var readyPatterns = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^(>|\$|%|❯)\s*$`) })

// loginShells are the shell names BuildRemoteCommand execs into after the dispatched agent exits.
var loginShells = map[string]bool{"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true, "ksh": true, "tcsh": true, "csh": true}

// isShellAfterExit reports a pane whose foreground process fell back to a login shell after the
// agent it was launched for exited (BuildRemoteCommand always execs "${SHELL:-/bin/sh}" -l on exit).
func isShellAfterExit(c PaneCapture) bool {
	return c.Agent != "" && loginShells[strings.TrimPrefix(c.CurrentCmd, "-")]
}

// lastNonEmptyLine returns the last line of s with any content, or "" if s is blank.
func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for _, line := range slices.Backward(lines) {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// normalizePane strips ANSI escapes and known animation noise so two samples of unchanged substantive
// content fingerprint identically even if a spinner or elapsed-time counter ticked between them.
func normalizePane(raw string) string {
	s := ansiRE().ReplaceAllString(raw, "")
	s = spinnerRE().ReplaceAllString(s, "·")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.Join(lines, "\n")
}

// fingerprintPane returns a bounded (16 hex char) hash of normalized pane text — never the text itself.
func fingerprintPane(normalized string) string {
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])[:16]
}

// classifyPane maps one pane capture against its previous fingerprint to a state, a human-readable
// reason, and the capture's own bounded fingerprint (fed back into the next ObservePanes call).
func classifyPane(prevFingerprint string, c PaneCapture) (state, reason, fingerprint string) {
	norm := normalizePane(c.Text)
	fingerprint = fingerprintPane(norm)
	switch {
	case isShellAfterExit(c):
		return StateReady, "agent exited; login shell idle at prompt", fingerprint
	case inputPatterns().MatchString(norm):
		return StateInputNeeded, "explicit input prompt detected", fingerprint
	case workingPatterns().MatchString(norm):
		return StateWorking, "working indicator detected", fingerprint
	case prevFingerprint == "":
		return StateUnknown, "first observation; no baseline yet", fingerprint
	case fingerprint != prevFingerprint:
		return StateWorking, "pane output changed since last sample", fingerprint
	case readyPatterns().MatchString(lastNonEmptyLine(norm)):
		return StateReady, "settled prompt, output unchanged", fingerprint
	default:
		return StateUnknown, "static ambiguous output, not a recognized prompt", fingerprint
	}
}

// observeOne classifies one fresh capture against its previous status (zero value if new), carrying
// ChangedAt forward only while the classified state stays the same across samples.
func observeOne(old PaneStatus, c PaneCapture, now time.Time) PaneStatus {
	state, reason, fp := classifyPane(old.fingerprint, c)
	changedAt := now
	if old.PaneID == c.PaneID && old.State == state && !old.ChangedAt.IsZero() {
		changedAt = old.ChangedAt
	}
	return PaneStatus{
		Host: c.Host, Session: c.Session, PaneID: c.PaneID, Agent: c.Agent,
		State: state, Reason: reason, ObservedAt: now, ChangedAt: changedAt,
		fingerprint: fp,
	}
}

// markGone reports a previously observed pane that no longer appears in a successful capture.
func markGone(p PaneStatus, now time.Time) PaneStatus {
	if p.State != StateGone {
		p.ChangedAt = now
	}
	p.State, p.Reason, p.ObservedAt, p.fingerprint = StateGone, "pane no longer exists", now, ""
	return p
}

// degradeToError reports every previously known pane as unobservable rather than discarding it, so
// one transient ssh/tmux failure during repeated sampling doesn't blank out the caller's view.
func degradeToError(prev []PaneStatus, err error, now time.Time) []PaneStatus {
	out := make([]PaneStatus, 0, len(prev))
	for _, p := range prev {
		if p.State == StateGone { // already reported gone: dropped, never revived as an error entry
			continue
		}
		if p.State != StateError {
			p.ChangedAt = now
		}
		p.State, p.Reason, p.ObservedAt = StateError, err.Error(), now
		out = append(out, p)
	}
	return out
}

// ObservePanes advances prev (one host's last-known pane statuses) using a fresh capture pass: new panes
// are classified against their matching previous entry (by PaneID), vanished panes report gone, and
// a host-wide capture failure (captureErr != nil) degrades every previously known pane to error
// instead of dropping it. now and the capture/error inputs are both caller-supplied, so tests can
// fake tmux/ssh and the clock without exercising a real pane.
func ObservePanes(prev []PaneStatus, captures []PaneCapture, captureErr error, now time.Time) []PaneStatus {
	if captureErr != nil {
		return degradeToError(prev, captureErr, now)
	}
	byID := make(map[string]PaneStatus, len(prev))
	for _, p := range prev {
		byID[p.PaneID] = p
	}
	seen := make(map[string]bool, len(captures))
	out := make([]PaneStatus, 0, len(captures))
	for _, c := range captures {
		seen[c.PaneID] = true
		out = append(out, observeOne(byID[c.PaneID], c, now))
	}
	for id, p := range byID {
		if !seen[id] && p.State != StateGone { // a pane already reported gone is dropped on the next pass
			out = append(out, markGone(p, now))
		}
	}
	return out
}

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
// reuses tmux.go's fieldSep ("|") for the header columns: tmux's format engine replaces a real
// tab byte with "_" in its own -F output (see fieldSep's comment), so tab cannot separate these.
//
// list-panes's own exit status is checked explicitly, outside any pipe, before filtering/looping:
// piped straight into awk/while, a list-panes failure (exit nonzero, empty stdout) is indistinguishable
// from "no cc-* panes exist" (both yield zero loop iterations, and a while loop that never ran its
// body exits 0) — the ssh round trip reports success with an empty capture, and CapturePanes/ObservePanes
// then has no way to tell a transient tmux failure apart from every tracked pane having vanished. A
// capture-pane failure inside the loop instead `exit`s the loop (its subshell, piped as the script's
// last statement) immediately, which is still the pipeline's — and so the whole script's — exit status.
var captureCommand = `list_out="$(tmux list-panes -a -F '#{session_name}` + fieldSep + `#{pane_id}` + fieldSep +
	`#{@cc_agent}` + fieldSep + `#{pane_current_command}' 2>/dev/null)"; list_rc=$?` +
	`; if [ "$list_rc" -ne 0 ]; then echo 'network remote: tmux list-panes failed' >&2; exit 1; fi` +
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

// RemoteRunner executes argv (as built by ExecArgs) and returns its output; production code wraps
// exec.CommandContext, tests substitute a fake with no real ssh/tmux involved.
type RemoteRunner func(ctx context.Context, argv []string) ([]byte, error)

// ExecRunner is the production RemoteRunner.
func ExecRunner(ctx context.Context, argv []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = execWaitDelay
	return OutputCapped(cmd)
}

// capBuffer collects writes up to max, then fails the write so the child is cut off with memory bounded.
type capBuffer struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (w *capBuffer) Write(p []byte) (int, error) {
	if w.buf.Len()+len(p) > w.max {
		w.over = true
		return 0, fmt.Errorf("output over %d bytes", w.max)
	}
	return w.buf.Write(p)
}

// OutputCapped is cmd.Output() with stdout held to maxCaptureBytes: over it, the command fails with an over-cap error.
func OutputCapped(cmd *exec.Cmd) ([]byte, error) {
	stdout := &capBuffer{max: maxCaptureBytes}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = stdout, &stderr
	err := cmd.Run()
	if stdout.over { // the child's SIGPIPE exit would otherwise mask the cap
		return stdout.buf.Bytes(), fmt.Errorf("output over %d bytes", stdout.max)
	}
	if ee := (*exec.ExitError)(nil); errors.As(err, &ee) && ee.Stderr == nil {
		ee.Stderr = stderr.Bytes()
	}
	return stdout.buf.Bytes(), err
}

// CapturePanes lists and captures every cc-* tmux pane on host via run, bounding the round trip to
// captureTimeout and the result to maxCaptureBytes/maxPanes (over either is an error, never a silent cut); batch forbids ssh password prompts.
func CapturePanes(ctx context.Context, host string, local, batch bool, run RemoteRunner) ([]PaneCapture, error) {
	cctx, cancel := context.WithTimeout(ctx, captureTimeout)
	defer cancel()
	argv, err := ExecArgs(host, captureCommand, local, batch)
	if err != nil {
		return nil, fmt.Errorf("dispatch: capture panes on %s: %w", host, err)
	}
	out, err := run(cctx, argv)
	if err != nil {
		return nil, fmt.Errorf("dispatch: capture panes on %s: %w", host, err)
	}
	if len(out) > maxCaptureBytes {
		return nil, fmt.Errorf("dispatch: capture panes on %s: output over %d bytes", host, maxCaptureBytes)
	}
	caps := parseCaptureBlob(host, string(out))
	if len(caps) > maxPanes {
		return nil, fmt.Errorf("dispatch: capture panes on %s: more than %d panes", host, maxPanes)
	}
	return caps, nil
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
		if len(caps) > maxPanes {
			break
		}
		header, text, ok := strings.Cut(rec, "\n")
		if !ok {
			continue
		}
		text, _, ok = strings.Cut(text, paneRecordEnd)
		if !ok {
			continue
		}
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
