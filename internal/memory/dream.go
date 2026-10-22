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

const dreamInterval = 24 * time.Hour

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
	Timeout time.Duration // 0 = HarvestTimeout
}

func agentDreamPrompt() string {
	body, err := os.ReadFile(filepath.Join(paths.ConfigDir(), "commands", "reflection", "consolidate.md"))
	text := string(body)
	if i := strings.Index(text, "## Part 2: Dream Consolidation"); err == nil && i >= 0 {
		text = text[i:]
	} else {
		text = ""
	}
	return "Background daily consolidation of the memory bank. Resolve the local bank with `claude-code memory path`; " +
		"the home bank is the memory root. Dedupe and merge; strike and archive superseded decisions into archive/; " +
		"keep every <!-- id:... --> marker; bring local and home conventions.md (80 lines) and decisionLog.md " +
		"(200 lines) under their caps. Never edit RULES regions and never edit CLAUDE.md. Be surgical.\n\n" + text +
		"\n\nOverride: you are the consolidator, so do the work yourself. Edit only files under the memory root; " +
		"never edit CLAUDE.md or rules/ — list any promotion candidates in your answer instead."
}

// Dream promotes local blocks to the home bank, consolidates both banks on the agent, settles, and stamps.
func Dream(o DreamOptions) error {
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Timeout == 0 {
		o.Timeout = HarvestTimeout
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
			ctx, cancel := context.WithTimeout(context.Background(), o.Timeout)
			defer cancel()
			_ = os.Setenv(HarvestChildEnv, "1") // a Claude fallback leg must not re-trigger hooks; cannot fail for a fixed key
			who, err := o.Run(ctx, agentDreamPrompt(), o.CWD)
			runErr = err
			fmt.Fprintln(o.Out, "memory dream: ran on", who)
		}
		if _, err := SettleBank(o.CWD, time.Now()); err != nil {
			fmt.Fprintln(o.Out, "memory dream: settle:", err)
		}
		return nil
	})
	stampErr := paths.AtomicWrite(DreamStampPath(), []byte(time.Now().Format(time.RFC3339)+"\n"), 0o600)
	return errors.Join(lockErr, runErr, stampErr)
}
