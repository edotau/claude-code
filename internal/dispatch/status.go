package dispatch

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"slices"
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
// Observe calls (bounded, never the captured text) and is deliberately not exported/serialized.
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

// Normalize strips ANSI escapes and known animation noise so two samples of unchanged substantive
// content fingerprint identically even if a spinner or elapsed-time counter ticked between them.
func Normalize(raw string) string {
	s := ansiRE().ReplaceAllString(raw, "")
	s = spinnerRE().ReplaceAllString(s, "·")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.Join(lines, "\n")
}

// Fingerprint returns a bounded (16 hex char) hash of normalized pane text — never the text itself.
func Fingerprint(normalized string) string {
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])[:16]
}

// Classify maps one pane capture against its previous fingerprint to a state, a human-readable
// reason, and the capture's own bounded fingerprint (fed back into the next Observe call).
func Classify(prevFingerprint string, c PaneCapture) (state, reason, fingerprint string) {
	norm := Normalize(c.Text)
	fingerprint = Fingerprint(norm)
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
	state, reason, fp := Classify(old.fingerprint, c)
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
	out := make([]PaneStatus, len(prev))
	for i, p := range prev {
		if p.State != StateError {
			p.ChangedAt = now
		}
		p.State, p.Reason, p.ObservedAt = StateError, err.Error(), now
		out[i] = p
	}
	return out
}

// Observe advances prev (one host's last-known pane statuses) using a fresh capture pass: new panes
// are classified against their matching previous entry (by PaneID), vanished panes report gone, and
// a host-wide capture failure (captureErr != nil) degrades every previously known pane to error
// instead of dropping it. now and the capture/error inputs are both caller-supplied, so tests can
// fake tmux/ssh and the clock without exercising a real pane.
func Observe(prev []PaneStatus, captures []PaneCapture, captureErr error, now time.Time) []PaneStatus {
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
		if !seen[id] {
			out = append(out, markGone(p, now))
		}
	}
	return out
}
