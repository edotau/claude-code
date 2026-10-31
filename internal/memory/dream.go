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

// DreamPreview is `memory dream --dry-run`'s deterministic report: what the mechanical steps would
// change, with no model call and no write. The judgment steps (dedup, merge, cross-pollinate) are
// never previewed here — they need a live read, not a count.
type DreamPreview struct {
	Dir           string
	Oversized     []string // local bank files over cap
	HomeOversized []string // home bank files over cap (only when local isn't already the home bank)
	Promotable    int      // local overflow blocks that would move to the home bank
	Archivable    int      // overflow blocks that would move to archive/ (home bank has nowhere else to go)
	SessionRotate int      // sessionHistory entries that would rotate to archive/
}

// Any reports whether the preview found anything a real dream run would change mechanically.
func (p DreamPreview) Any() bool {
	return len(p.Oversized) > 0 || len(p.HomeOversized) > 0 || p.Promotable > 0 || p.Archivable > 0 || p.SessionRotate > 0
}

// Report renders the preview as `memory dream --dry-run`'s stdout.
func (p DreamPreview) Report() string {
	var b strings.Builder
	fmt.Fprintf(&b, "bank: %s\n", p.Dir)
	if len(p.Oversized) > 0 {
		fmt.Fprintf(&b, "over cap (local): %s\n", strings.Join(p.Oversized, ", "))
	}
	if len(p.HomeOversized) > 0 {
		fmt.Fprintf(&b, "over cap (home): %s\n", strings.Join(p.HomeOversized, ", "))
	}
	if p.Promotable > 0 {
		fmt.Fprintf(&b, "would promote %d block(s) local -> home\n", p.Promotable)
	}
	if p.Archivable > 0 {
		fmt.Fprintf(&b, "would archive %d block(s)\n", p.Archivable)
	}
	if p.SessionRotate > 0 {
		fmt.Fprintf(&b, "would rotate %d sessionHistory entrie(s) to archive/\n", p.SessionRotate)
	}
	if !p.Any() {
		b.WriteString("nothing to consolidate mechanically — a real run still applies the judgment pass (dedup, merge, cross-pollinate)\n")
	}
	return b.String()
}

// PreviewDream computes DreamPreview for root without writing anything — the preview a live model
// never needs to run itself.
func PreviewDream(root string, now time.Time) DreamPreview {
	dir := Dir(root)
	p := DreamPreview{Dir: dir, Oversized: Stat(root).Oversized}
	if !IsHomeBank(root) {
		p.HomeOversized = HomeBankStat().Oversized
	}
	for name, capLines := range promoteCaps {
		n, _ := previewOverflowCount(filepath.Join(dir, name), capLines)
		if IsHomeBank(root) {
			p.Archivable += n
		} else {
			p.Promotable += n
		}
	}
	p.SessionRotate = previewSessionRotate(dir)
	return p
}

// previewOverflowCount is promoteFile/archiveFile's split, read-only: how many oldest blocks would
// overflow path's cap.
func previewOverflowCount(path string, capLines int) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, nil // absent file — nothing to preview
	}
	if LogicalLineCount(string(data)) <= capLines {
		return 0, nil
	}
	base := filepath.Base(path)
	header, blocks := entryBlocks(base, string(data))
	_, overflow := settledSplit(base, header, blocks, capLines)
	return len(overflow), nil
}

// previewSessionRotate is RotateSessionHistory's cap check, read-only.
func previewSessionRotate(dir string) int {
	data, err := os.ReadFile(filepath.Join(dir, "sessionHistory.md"))
	if err != nil {
		return 0
	}
	_, entries := splitHeadingBlocks(string(data))
	if len(entries) <= SessionHistoryMaxEntries {
		return 0
	}
	return len(entries) - SessionHistoryMaxEntries
}

const dreamInterval = 24 * time.Hour

// DreamChildEnv marks a process spawned by `memory dream`'s agent run — the one context where
// `memory promote --to claude-md` is allowed to print its gated procedure.
const DreamChildEnv = "CLAUDE_MEMORY_DREAM_CHILD"

// IsDreamChild reports whether this process runs inside a dream worker (or one of its children).
func IsDreamChild() bool {
	v := os.Getenv(DreamChildEnv)
	return v != "" && v != "0"
}

// DreamStampPath records the last consolidation time.
func DreamStampPath() string { return filepath.Join(StateDir(), "last-dream") }

func dreamStamp() (time.Time, bool) {
	data, err := os.ReadFile(DreamStampPath())
	if err != nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data)))
	return t, err == nil
}

// DreamDue: the last dream is over a day old (or never ran) and a bank changed since.
func DreamDue(cwd string, now time.Time) bool {
	stamp, ok := dreamStamp()
	if ok && now.Sub(stamp) < dreamInterval {
		return false
	}
	return NewestMtime(Dir(cwd)).After(stamp) || NewestMtime(HomeBankDir()).After(stamp)
}

// DreamOptions drive one consolidation.
type DreamOptions struct {
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

// Dream promotes local blocks to the home bank, consolidates both banks on the agent, settles, and stamps.
func Dream(o DreamOptions) error {
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Timeout == 0 {
		o.Timeout = UpdateTimeout
	}
	var runErr error
	lockErr := paths.WithFileLock(lockPath(Dir(o.CWD)), func() error {
		now := time.Now()
		n, err := PromoteLocalToGlobal(o.CWD, now)
		if err != nil {
			fmt.Fprintln(o.Out, "memory dream: promote:", err)
		}
		fmt.Fprintf(o.Out, "memory dream: promoted %d block(s) to home\n", n)
		if o.Run != nil {
			prompt, perr := agentDreamPrompt()
			if perr != nil {
				runErr = perr
			} else {
				ctx, cancel := context.WithTimeout(context.Background(), o.Timeout)
				defer cancel()
				_ = os.Setenv(UpdateChildEnv, "1") // a Claude fallback dispatch must not re-trigger hooks; cannot fail for a fixed key
				_ = os.Setenv(DreamChildEnv, "1")  // lets this run's own `memory promote --to claude-md` through the gate
				who, err := o.Run(ctx, prompt, o.CWD)
				runErr = err
				fmt.Fprintln(o.Out, "memory dream: ran on", who)
			}
		}
		return nil
	})
	// Settle after the lock is released.
	if _, err := SettleBank(o.CWD, time.Now()); err != nil {
		fmt.Fprintln(o.Out, "memory dream: settle:", err)
	}
	stampErr := paths.AtomicWrite(DreamStampPath(), []byte(time.Now().Format(time.RFC3339)+"\n"), 0o600)
	return errors.Join(lockErr, runErr, stampErr)
}
