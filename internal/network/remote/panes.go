package remote

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/proc"
)

// Pane states; a static pane is unknown, never idle: this package does not claim a pane is idle.
const (
	stateWorking     = "working"      // output changing, or an explicit working indicator present
	stateInputNeeded = "input-needed" // an explicit prompt (yes/no, composer box) is waiting on the human
	stateReady       = "ready"        // a settled, recognized prompt with unchanged output
	stateUnknown     = "unknown"      // static output with no recognizable prompt; never called idle
	stateGone        = "gone"         // the pane no longer exists in a successful capture
	stateError       = "error"        // the host-wide capture failed (ssh/tmux error)
)

// PaneStatus is one pane's classified state; fingerprint (bounded, never the text) carries across samples unexported.
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

// ansiRE strips cursor/SGR escapes and bare CRs, in case a capture includes them (-e or a future caller).
var ansiRE = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]|\x1b\][^\x07]*\x07|\r`)
})

// spinnerRE removes per-frame noise: braille spinner glyphs and elapsed-seconds counters like "(12s · esc …)".
var spinnerRE = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`[\x{2800}-\x{28FF}]|\(\d+s[^)]*\)|\b\d+s\b`)
})

// inputPatterns match a request for the human: a yes/no or numbered prompt, a boxed TUI composer, "press enter".
var inputPatterns = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?mi)\(y/n\)|\[y/n\]|do you want to proceed|press enter to continue|` +
		`waiting for (your )?input|^\s*1\.\s*yes|[╭╮╰╯]|│\s*>`)
})

// workingPatterns match text an agent CLI prints while actively generating or running a tool.
var workingPatterns = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(
		`(?i)esc to interrupt|thinking…|thinking\.\.\.|generating|tokens/s|running tool|ctrl-c to stop`)
})

// readyPatterns match a settled, idle prompt: the agent's bare composer caret or a plain shell prompt.
var readyPatterns = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^(>|\$|%|❯)\s*$`) })

// loginShells are the shell names BuildCommand execs into after the dispatched agent exits.
var loginShells = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true, "ksh": true, "tcsh": true, "csh": true,
}

// isShellAfterExit reports a pane back at a login shell after its agent exited (BuildCommand execs $SHELL -l).
func isShellAfterExit(c paneCapture) bool {
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

// normalizePane strips ANSI and animation noise so unchanged content fingerprints the same across samples.
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

// classifyPane maps a capture and its previous fingerprint to a state, a reason and the new fingerprint.
func classifyPane(prevFingerprint string, c paneCapture) (state, reason, fingerprint string) {
	norm := normalizePane(c.Text)
	fingerprint = fingerprintPane(norm)
	switch {
	case isShellAfterExit(c):
		return stateReady, "agent exited; login shell idle at prompt", fingerprint
	case inputPatterns().MatchString(norm):
		return stateInputNeeded, "explicit input prompt detected", fingerprint
	case workingPatterns().MatchString(norm):
		return stateWorking, "working indicator detected", fingerprint
	case prevFingerprint == "":
		return stateUnknown, "first observation; no baseline yet", fingerprint
	case fingerprint != prevFingerprint:
		return stateWorking, "pane output changed since last sample", fingerprint
	case readyPatterns().MatchString(lastNonEmptyLine(norm)):
		return stateReady, "settled prompt, output unchanged", fingerprint
	default:
		return stateUnknown, "static ambiguous output, not a recognized prompt", fingerprint
	}
}

// observeOne classifies a fresh capture against its previous status, keeping ChangedAt while the state holds.
func observeOne(old PaneStatus, c paneCapture, now time.Time) PaneStatus {
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
	if p.State != stateGone {
		p.ChangedAt = now
	}
	p.State, p.Reason, p.ObservedAt, p.fingerprint = stateGone, "pane no longer exists", now, ""
	return p
}

// degradeToError marks every known pane unobservable rather than dropping it, so one ssh blip keeps the view.
func degradeToError(prev []PaneStatus, err error, now time.Time) []PaneStatus {
	out := make([]PaneStatus, 0, len(prev))
	for _, p := range prev {
		if p.State == stateGone { // already reported gone: dropped, never revived as an error entry
			continue
		}
		if p.State != stateError {
			p.ChangedAt = now
		}
		p.State, p.Reason, p.ObservedAt = stateError, err.Error(), now
		out = append(out, p)
	}
	return out
}

// observePanes advances prev with a capture pass: match by PaneID, vanished = gone, capture failure = error.
func observePanes(prev []PaneStatus, captures []paneCapture, captureErr error, now time.Time) []PaneStatus {
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
		if !seen[id] && p.State != stateGone { // a pane already reported gone is dropped on the next pass
			out = append(out, markGone(p, now))
		}
	}
	return out
}

// paneRecordStart/End frame each pane's capture with SOH/STX, bytes rendered terminal text never holds.
const (
	paneRecordStart = "\x01"
	paneRecordEnd   = "\x02"
)

const (
	paneCaptureLines = 50               // trailing lines tmux captures per pane
	maxPanes         = 64               // bounds how many panes one call parses
	captureTimeout   = 20 * time.Second // bounds one host's whole list+capture round trip
)

// captureCommand lists cc-* panes and captures each, control-byte framed; fieldSep is "|" since tmux mangles tabs.
// Gotcha: list-panes' exit is checked before the pipe, else a tmux failure reads as no panes and all look gone.
var captureCommand = `list_out="$(tmux list-panes -a -F '#{session_name}` + fieldSep + `#{pane_id}` + fieldSep +
	`#{@cc_agent}` + fieldSep + `#{pane_current_command}' 2>/dev/null)"; list_rc=$?` +
	`; if [ "$list_rc" -ne 0 ]; then echo 'network remote: tmux list-panes failed' >&2; exit 1; fi` +
	`; printf '%s\n' "$list_out"` +
	` | awk -F'` + fieldSep + `' '$1 ~ /^cc-/'` +
	` | while IFS='` + fieldSep + `' read -r sess pid agent cmd; do` +
	` printf '` + paneRecordStart + `%s` + fieldSep + `%s` + fieldSep + `%s` + fieldSep +
	`%s\n' "$sess" "$pid" "$agent" "$cmd";` +
	` if ! tmux capture-pane -p -t "$pid" -S -` + strconv.Itoa(paneCaptureLines) + ` 2>/dev/null; then exit 1; fi;` +
	` printf '` + paneRecordEnd + `\n'; done`

// paneCapture is one pane's raw sample: tmux-reported identity plus its last captured lines.
type paneCapture struct {
	Host, Session, PaneID, Agent, CurrentCmd string
	Text                                     string
}

// paneRunner runs an ExecArgs argv and returns its output; tests swap in a fake with no ssh or tmux.
type paneRunner func(ctx context.Context, argv []string) ([]byte, error)

// execRunner is the production paneRunner.
func execRunner(ctx context.Context, argv []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = proc.ExecWaitDelay
	return proc.OutputCapped(cmd)
}

// capturePanes captures every cc-* pane on host via run; over captureTimeout, proc.MaxCaptureBytes or maxPanes errors.
func capturePanes(ctx context.Context, host string, local, batch bool, run paneRunner) ([]paneCapture, error) {
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
	if len(out) > proc.MaxCaptureBytes {
		return nil, fmt.Errorf("dispatch: capture panes on %s: output over %d bytes", host, proc.MaxCaptureBytes)
	}
	caps := parseCaptureBlob(host, string(out))
	if len(caps) > maxPanes {
		return nil, fmt.Errorf("dispatch: capture panes on %s: more than %d panes", host, maxPanes)
	}
	return caps, nil
}

// parseCaptureBlob splits captureCommand's framed output into records, skipping malformed ones.
func parseCaptureBlob(host, blob string) []paneCapture {
	parts := strings.Split(blob, paneRecordStart)
	caps := make([]paneCapture, 0, len(parts))
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
		caps = append(caps, paneCapture{
			Host: host, Session: cols[0], PaneID: cols[1], Agent: cols[2], CurrentCmd: cols[3],
			Text: strings.TrimRight(text, "\n"),
		})
	}
	return caps
}
