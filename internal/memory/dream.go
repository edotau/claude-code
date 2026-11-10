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

// dreamPreview is `memory dream --dry-run`'s deterministic report: what the mechanical steps would
// change, with no model call and no write. The judgment steps (dedup, merge, cross-pollinate) are
// never previewed here — they need a live read, not a count.
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

// previewDream computes dreamPreview for root without writing anything — the preview a live model
// never needs to run itself.
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

// dreamRetry is how long a failed dream waits before DreamDue offers it again.
const dreamRetry = 6 * time.Hour

// dreamChildEnv marks a process spawned by `memory dream`'s agent run — the one context where
// `memory promote --to claude-md` is allowed to print its gated procedure.
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

// DreamDue: the last dream is over a day old (or never ran) and a bank changed since; a failed run is due at its backdated stamp.
func DreamDue(cwd string, now time.Time) bool {
	stamp, failed, ok := dreamStamp(cwd)
	if ok && now.Sub(stamp) < dreamInterval {
		return false
	}
	if ok && failed {
		return true
	}
	return NewestMtime(Dir(cwd)).After(stamp) || NewestMtime(HomeBankDir()).After(stamp)
}

// dreamOptions drive one consolidation.
type dreamOptions struct {
	CWD     string
	Run     AgentFunc
	Out     io.Writer
	Timeout time.Duration // 0 = UpdateTimeout
}

// agentDreamPrompt frames the model-residue consolidation pass; the procedure itself is one copy,
// embedded at prompts/dream.md.
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
	if o.Timeout == 0 {
		o.Timeout = UpdateTimeout
	}
	var runErr, archiveErr error
	lockErr := paths.WithFileLock(lockPath(Dir(o.CWD)), func() error {
		now := time.Now()
		n, err := archiveOverflow(o.CWD, now)
		if err != nil {
			archiveErr = fmt.Errorf("archive: %w", err)
			fmt.Fprintln(o.Out, "memory dream:", archiveErr)
		}
		fmt.Fprintf(o.Out, "memory dream: archived %d block(s)\n", n)
		if o.Run != nil {
			prompt, perr := agentDreamPrompt()
			if perr != nil {
				runErr = perr
			} else {
				ctx, cancel := context.WithTimeout(context.Background(), o.Timeout)
				defer cancel()
				_ = os.Setenv(UpdateChildEnv, "1") // a Claude fallback dispatch must not re-trigger hooks
				_ = os.Setenv(dreamChildEnv, "1")  // lets this run's own `memory promote --to claude-md` through the gate
				who, err := o.Run(ctx, prompt, o.CWD)
				runErr = err
				fmt.Fprintln(o.Out, "memory dream: ran on", who)
			}
		}
		return nil
	})
	// Settle after the lock is released.
	var settleErr error
	if _, err := settleBank(o.CWD, time.Now()); err != nil {
		settleErr = fmt.Errorf("settle: %w", err)
		fmt.Fprintln(o.Out, "memory dream:", settleErr)
	}
	if err := errors.Join(lockErr, runErr, archiveErr, settleErr); err != nil {
		// backdated, so DreamDue retries in dreamRetry
		return errors.Join(err, stampDream(o.CWD, time.Now().Add(dreamRetry-dreamInterval), true))
	}
	return stampDream(o.CWD, time.Now(), false)
}

func stampDream(root string, at time.Time, failed bool) error {
	body := at.Format(time.RFC3339) + "\n"
	if failed {
		body += "failed\n"
	}
	return paths.AtomicWrite(dreamStampPath(root), []byte(body), 0o600)
}
