package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/transcript"
)

// dreamPreview is `memory dream --dry-run`'s deterministic report of the mechanical steps only.
type dreamPreview struct {
	Dir           string
	Oversized     []string // bank files over cap
	Archivable    int      // durable-file overflow blocks that would move to this store's archive/
	SessionRotate int      // sessionHistory entries that would rotate to archive/
	Unreadable    []string // "name: error" for each bank file that could not be read, so was not checked
}

// Any reports whether the preview found anything a real dream run would change mechanically.
func (p dreamPreview) Any() bool {
	return len(p.Oversized) > 0 || p.Archivable > 0 || p.SessionRotate > 0
}

// Report renders the preview as `memory dream --dry-run`'s stdout.
func (p dreamPreview) Report() string {
	var b strings.Builder
	fmt.Fprintf(&b, "bank: %s\n", p.Dir)
	if len(p.Oversized) > 0 {
		fmt.Fprintf(&b, "over cap: %s\n", strings.Join(p.Oversized, ", "))
	}
	if p.Archivable > 0 {
		fmt.Fprintf(&b, "would archive %d block(s)\n", p.Archivable)
	}
	if p.SessionRotate > 0 {
		fmt.Fprintf(&b, "would rotate %d sessionHistory entr(ies) to archive/\n", p.SessionRotate)
	}
	for _, u := range p.Unreadable {
		fmt.Fprintf(&b, "home %s — unreadable, so not checked\n", u)
	}
	if !p.Any() && len(p.Unreadable) == 0 {
		b.WriteString("nothing to consolidate mechanically — a real run still applies the judgment pass (dedup, merge, cross-pollinate)\n")
	}
	return b.String()
}

// previewDream computes dreamPreview without writing anything.
func previewDream() dreamPreview {
	local := statBank() // one read per typed file feeds the cap, archive and rotation checks below
	p := dreamPreview{Dir: SubDir(""), Oversized: local.Oversized, Unreadable: local.Unreadable}
	for name, capLines := range archiveCaps {
		_, _, overflow := overflowSplit(name, string(local.data[name]), capLines) // archiveFile's split, read-only
		p.Archivable += len(overflow)
	}
	_, _, rotate := sessionOverflow(string(local.data["sessionHistory.md"]))
	p.SessionRotate = len(rotate)
	return p
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
func dreamStampPath() string {
	return filepath.Join(StateDir(), "last-dream-"+stateKey())
}

// dreamStamp reads the stamp: its time (line 1) and whether that run failed (line 2 = "failed").
func dreamStamp() (t time.Time, failed, ok bool) {
	data, err := os.ReadFile(dreamStampPath())
	if err != nil {
		return time.Time{}, false, false
	}
	first, rest, _ := strings.Cut(strings.TrimSpace(string(data)), "\n")
	t, err = time.Parse(time.RFC3339, first)
	return t, rest == "failed", err == nil
}

// effectiveStamp is dreamStamp, but a future stamp (clock moved back) reads as none: it would hide every changed file.
func effectiveStamp(now time.Time) (t time.Time, failed, ok bool) {
	if t, failed, ok = dreamStamp(); ok && t.After(now) {
		return time.Time{}, false, false
	}
	return t, failed, ok
}

// dreamDue: the last dream is over a day old (or never ran) and a bank changed since; a failed run is due at its stamp.
func dreamDue(now time.Time) bool {
	stamp, failed, ok := effectiveStamp(now)
	if ok && now.Sub(stamp) < dreamInterval {
		return false
	}
	if ok && failed {
		return true
	}
	return newestMtime().After(stamp)
}

// dreamOptions drive one consolidation.
type dreamOptions struct {
	CWD   string
	Run   AgentFunc
	Out   io.Writer
	IfDue bool // re-check dreamDue once the lock is held: a racing peer may have just dreamed
}

// dreamChanged lists the top-level and feedback/ and notes/ store .md files modified after since (all when zero).
func dreamChanged(since time.Time) []string {
	var out []string
	for _, dir := range []string{StoreDir(), filepath.Join(StoreDir(), "feedback"), filepath.Join(StoreDir(), "notes")} {
		for _, f := range transcript.GlobIn(dir, "*.md") {
			if fi, err := os.Stat(f); err == nil && fi.ModTime().After(since) {
				out = append(out, f)
			}
		}
	}
	return out
}

// agentDreamPrompt frames the consolidation pass; the procedure is one copy, embedded at prompts/dream.md.
func agentDreamPrompt(changed []string) (string, error) {
	ref, err := Reference("dream")
	if err != nil {
		return "", err
	}
	list := "none — nothing changed since the last dream"
	if len(changed) > 0 {
		list = "\n- " + strings.Join(changed, "\n- ")
	}
	return fmt.Sprintf(dreamPromptFormat, StoreDir()) +
		"Write only under the memory root. Be surgical.\n\nChanged since the last dream: " + list + "\n\n" + ref, nil
}

// dream archives durable-file overflow, consolidates the bank on the agent, settles, and stamps.
func dream(o dreamOptions) error {
	if o.Out == nil {
		o.Out = io.Discard
	}
	var runErr, archiveErr error
	notDue := false
	lockErr := paths.WithFileLock(lockPathOf(), func() error {
		if notDue = o.IfDue && !dreamDue(time.Now()); notDue {
			fmt.Fprintln(o.Out, "memory dream: not due")
			return nil
		}
		runErr, archiveErr = dreamLocked(o)
		return nil
	})
	if notDue && lockErr == nil {
		return nil
	}
	if err := errors.Join(lockErr, runErr, archiveErr, dreamSettle(o)); err != nil { // settle after the lock is released
		// backdated, so dreamDue retries in dreamRetry
		return errors.Join(err, stampDream(time.Now().Add(dreamRetry-dreamInterval), true))
	}
	return stampDream(time.Now(), false)
}

// dreamSettle settles the bank and reports a failure on o.Out as well as returning it.
func dreamSettle(o dreamOptions) error {
	if _, err := settleBank(time.Now()); err != nil {
		settleErr := fmt.Errorf("settle: %w", err)
		fmt.Fprintln(o.Out, "memory dream:", settleErr)
		return settleErr
	}
	refreshNotice(o.Out, "memory dream", o.CWD)
	return nil
}

// dreamLocked archives overflow and runs the agent, the bank lock held; it returns the agent and archive errors.
func dreamLocked(o dreamOptions) (runErr, archiveErr error) {
	n, err := archiveOverflow(time.Now())
	if err != nil {
		archiveErr = fmt.Errorf("archive: %w", err)
		fmt.Fprintln(o.Out, "memory dream:", archiveErr)
	}
	fmt.Fprintf(o.Out, "memory dream: archived %d block(s)\n", n)
	if o.Run == nil {
		return nil, archiveErr
	}
	since, _, _ := effectiveStamp(time.Now()) // no stamp: the zero time lists every file
	changed := dreamChanged(since)
	prompt, err := agentDreamPrompt(changed)
	if err != nil {
		return err, archiveErr
	}
	plan := startPlanPhase(o.CWD)
	prompt += plan.promptBlock()
	defer plan.finish(o.Out)
	ctx, cancel := context.WithTimeout(context.Background(), updateTimeout)
	defer cancel()
	// UpdateChildEnv keeps a fallback dispatch from re-triggering hooks; dreamChildEnv lets `memory promote` through
	start := time.Now()
	var blocks int
	ctx = context.WithValue(ctx, gateBlocksKey{}, &blocks)
	who, err := o.Run(withChildEnv(ctx, UpdateChildEnv+"=1", dreamChildEnv+"=1"), prompt, o.CWD)
	fmt.Fprintln(o.Out, "memory dream: ran on", who)
	if err == nil && len(changed) > 0 && len(dreamChanged(start)) == 0 {
		if blocks > 0 { // refused, not idle: fail so the stamp backdates and the dream retries
			err = fmt.Errorf("the gate refused %d call(s) and no bank file was edited; the dream will retry", blocks)
			fmt.Fprintln(o.Out, "memory dream:", err)
			return err, archiveErr
		}
		fmt.Fprintf(o.Out, "memory dream: the agent edited no bank file (%d changed since the last dream); see its reply\n", len(changed))
	}
	return err, archiveErr
}

// dreamPlans is the plan phase: the session checkout and its plans and citable evidence from before the run.
type dreamPlans struct {
	root   string
	before plans.Snapshot
	cite   plans.Cite
}

// startPlanPhase snapshots the checkout's plans; nil when cwd is no checkout or has no plans home.
func startPlanPhase(cwd string) *dreamPlans {
	top := GitToplevel(cwd)
	if top == "" {
		return nil
	}
	if _, err := os.Stat(plans.Home(top)); err != nil {
		return nil
	}
	return &dreamPlans{root: top, before: plans.TakeSnapshot(top), cite: storeCites()}
}

// promptBlock is the open tasks the plan phase judges, as compact JSON; empty when no recent plan has one.
func (d *dreamPlans) promptBlock() string {
	if d == nil {
		return ""
	}
	open := plans.OpenTasks(d.root)
	b, err := json.Marshal(open)
	if len(open) == 0 || err != nil {
		return ""
	}
	return "\n\nOpen plan tasks: " + string(b) + "\n"
}

// finish re-checks every plan change made during the run and prints one line; it never fails the dream.
func (d *dreamPlans) finish(out io.Writer) {
	if d == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	rep, err := plans.RecheckJob(ctx, d.root, JobDream, d.before, d.cite)
	if err == nil && len(rep.Kept)+len(rep.Reverted)+len(rep.Flagged) == 0 && !rep.TextChanged {
		return
	}
	line := rep.Line()
	if err != nil {
		line += "; re-check stopped: " + strings.Join(strings.Fields(err.Error()), " ")
	}
	fmt.Fprintln(out, "memory dream:", line)
}

// storeCites is the evidence a strike may cite, read before the run so the job cannot cite what it wrote.
func storeCites() plans.Cite {
	ids := map[string]bool{}
	logs := append([]string{Path("decisionLog.md")}, transcript.GlobIn(filepath.Join(StoreDir(), "archive"), "decisionLog*.md")...)
	for _, f := range logs {
		b, _ := os.ReadFile(f) // a missing log cites nothing
		for _, m := range idMarker().FindAllStringSubmatch(string(b), -1) {
			ids[m[1]] = true
		}
	}
	notes := map[string]bool{}
	for _, dir := range []string{"feedback", "notes"} {
		for _, f := range transcript.GlobIn(filepath.Join(StoreDir(), dir), "*.md") {
			notes[dir+"/"+filepath.Base(f)] = true
		}
	}
	return func(kind, ref string) bool { return kind == "decision" && ids[ref] || kind == "note" && notes[ref] }
}

func stampDream(at time.Time, failed bool) error {
	body := at.Format(time.RFC3339) + "\n"
	if failed {
		body += "failed\n"
	}
	return paths.AtomicWrite(dreamStampPath(), []byte(body), 0o600)
}
