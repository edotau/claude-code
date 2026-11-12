package memory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// dreamPreview is `memory dream --dry-run`'s deterministic report of the mechanical steps only.
type dreamPreview struct {
	Dir           string
	Oversized     []string // local bank files over cap
	HomeOversized []string // home bank files over cap (only when local isn't already the home bank)
	Archivable    int      // durable-file overflow blocks that would move to this store's archive/
	SessionRotate int      // sessionHistory entries that would rotate to archive/
	Unreadable    []string // "local|home name: error" for each bank file that could not be read, so was not checked
}

// Any reports whether the preview found anything a real dream run would change mechanically.
func (p dreamPreview) Any() bool {
	return len(p.Oversized) > 0 || len(p.HomeOversized) > 0 || p.Archivable > 0 || p.SessionRotate > 0
}

// Report renders the preview as `memory dream --dry-run`'s stdout.
func (p dreamPreview) Report() string {
	var b strings.Builder
	fmt.Fprintf(&b, "bank: %s\n", p.Dir)
	if len(p.Oversized) > 0 {
		fmt.Fprintf(&b, "over cap (local): %s\n", strings.Join(p.Oversized, ", "))
	}
	if len(p.HomeOversized) > 0 {
		fmt.Fprintf(&b, "over cap (home): %s\n", strings.Join(p.HomeOversized, ", "))
	}
	if p.Archivable > 0 {
		fmt.Fprintf(&b, "would archive %d block(s)\n", p.Archivable)
	}
	if p.SessionRotate > 0 {
		fmt.Fprintf(&b, "would rotate %d sessionHistory entr(ies) to archive/\n", p.SessionRotate)
	}
	for _, u := range p.Unreadable {
		fmt.Fprintf(&b, "%s — unreadable, so not checked\n", u)
	}
	if !p.Any() && len(p.Unreadable) == 0 {
		b.WriteString("nothing to consolidate mechanically — a real run still applies the judgment pass (dedup, merge, cross-pollinate)\n")
	}
	return b.String()
}

// previewDream computes dreamPreview for root without writing anything.
func previewDream(root string) dreamPreview {
	dir := Dir(root)
	local := statBank(dir) // one read per typed file feeds the cap, archive and rotation checks below
	p := dreamPreview{Dir: dir, Oversized: local.Oversized, Unreadable: tiered("local", local.Unreadable)}
	if !isHomeBank(root) {
		home := statBank(HomeBankDir())
		p.HomeOversized, p.Unreadable = home.Oversized, append(p.Unreadable, tiered("home", home.Unreadable)...)
	}
	for name, capLines := range archiveCaps {
		_, _, overflow := overflowSplit(name, string(local.data[name]), capLines) // archiveFile's split, read-only
		p.Archivable += len(overflow)
	}
	_, _, rotate := sessionOverflow(string(local.data["sessionHistory.md"]))
	p.SessionRotate = len(rotate)
	return p
}

// tiered prefixes each of statBank's unreadable entries with its bank tier, the doctor's wording.
func tiered(tier string, unreadable []string) []string {
	out := make([]string, 0, len(unreadable))
	for _, u := range unreadable {
		out = append(out, tier+" "+u)
	}
	return out
}

const dreamInterval = 24 * time.Hour

// dreamRetry is how long a failed dream waits before dreamDue offers it again.
const dreamRetry = 6 * time.Hour

// dreamChildEnv marks a process spawned by dream's agent run, where `promote --to claude-md` may print its procedure.
const dreamChildEnv = "CLAUDE_MEMORY_DREAM_CHILD"

// isDreamChild reports whether this process runs inside a dream worker (or one of its children).
func isDreamChild() bool {
	v := os.Getenv(dreamChildEnv)
	return v != "" && v != "0"
}

// dreamStampPath records the last consolidation time.
func dreamStampPath(root string) string {
	return filepath.Join(StateDir(), "last-dream-"+filepath.Base(Dir(root)))
}

// dreamStamp reads the stamp: its time (line 1) and whether that run failed (line 2 = "failed").
func dreamStamp(root string) (t time.Time, failed, ok bool) {
	data, err := os.ReadFile(dreamStampPath(root))
	if err != nil {
		return time.Time{}, false, false
	}
	first, rest, _ := strings.Cut(strings.TrimSpace(string(data)), "\n")
	t, err = time.Parse(time.RFC3339, first)
	return t, rest == "failed", err == nil
}

// dreamDue: the last dream is over a day old (or never ran) and a bank changed since; a failed run is due at its backdated stamp.
func dreamDue(cwd string, now time.Time) bool {
	stamp, failed, ok := dreamStamp(cwd)
	if ok && stamp.After(now) { // a future stamp (clock moved back) would block dreaming until it passes
		stamp, failed, ok = time.Time{}, false, false
	}
	if ok && now.Sub(stamp) < dreamInterval {
		return false
	}
	if ok && failed {
		return true
	}
	return newestMtime(Dir(cwd)).After(stamp) || newestMtime(HomeBankDir()).After(stamp)
}

// dreamOptions drive one consolidation.
type dreamOptions struct {
	CWD   string
	Run   AgentFunc
	Out   io.Writer
	IfDue bool // re-check dreamDue once the lock is held: a racing peer may have just dreamed
}

// agentDreamPrompt frames the consolidation pass; the procedure is one copy, embedded at prompts/dream.md.
func agentDreamPrompt() (string, error) {
	ref, err := Reference("dream")
	if err != nil {
		return "", err
	}
	return "Background daily consolidation of the memory bank. Resolve the local bank with `claude-code memory path`; " +
		"the home bank is the memory root. Be surgical.\n\n" + ref, nil
}

// dream archives durable-file overflow, consolidates both banks on the agent, settles, and stamps.
func dream(o dreamOptions) error {
	if o.Out == nil {
		o.Out = io.Discard
	}
	var runErr, archiveErr error
	notDue := false
	lockErr := paths.WithFileLock(lockPath(Dir(o.CWD)), func() error {
		ran, err := withHomeLock(Dir(o.CWD), func() error { // the agent edits both banks, so hold both locks
			if notDue = o.IfDue && !dreamDue(o.CWD, time.Now()); notDue {
				fmt.Fprintln(o.Out, "memory dream: not due")
				return nil
			}
			runErr, archiveErr = dreamLocked(o)
			return nil
		})
		if err == nil && !ran {
			err = errors.New("another update or dream holds the home bank lock")
		}
		return err
	})
	if notDue && lockErr == nil {
		return nil
	}
	if err := errors.Join(lockErr, runErr, archiveErr, dreamSettle(o)); err != nil { // settle after the lock is released
		// backdated, so dreamDue retries in dreamRetry
		return errors.Join(err, stampDream(o.CWD, time.Now().Add(dreamRetry-dreamInterval), true))
	}
	return stampDream(o.CWD, time.Now(), false)
}

// dreamSettle settles the bank and reports a failure on o.Out as well as returning it.
func dreamSettle(o dreamOptions) error {
	if _, err := settleBank(o.CWD, time.Now()); err != nil {
		settleErr := fmt.Errorf("settle: %w", err)
		fmt.Fprintln(o.Out, "memory dream:", settleErr)
		return settleErr
	}
	return nil
}

// dreamLocked archives overflow and runs the agent, both banks' locks held; it returns the agent and archive errors.
func dreamLocked(o dreamOptions) (runErr, archiveErr error) {
	n, err := archiveOverflow(o.CWD, time.Now())
	if err != nil {
		archiveErr = fmt.Errorf("archive: %w", err)
		fmt.Fprintln(o.Out, "memory dream:", archiveErr)
	}
	fmt.Fprintf(o.Out, "memory dream: archived %d block(s)\n", n)
	if o.Run == nil {
		return nil, archiveErr
	}
	prompt, err := agentDreamPrompt()
	if err != nil {
		return err, archiveErr
	}
	ctx, cancel := context.WithTimeout(context.Background(), updateTimeout)
	defer cancel()
	_ = os.Setenv(UpdateChildEnv, "1") // a Claude fallback dispatch must not re-trigger hooks
	_ = os.Setenv(dreamChildEnv, "1")  // lets this run's own `memory promote --to claude-md` through the gate
	who, err := o.Run(ctx, prompt, o.CWD)
	fmt.Fprintln(o.Out, "memory dream: ran on", who)
	return err, archiveErr
}

func stampDream(root string, at time.Time, failed bool) error {
	body := at.Format(time.RFC3339) + "\n"
	if failed {
		body += "failed\n"
	}
	return paths.AtomicWrite(dreamStampPath(root), []byte(body), 0o600)
}
