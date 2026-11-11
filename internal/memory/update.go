package memory

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// updatePrompt is the headless update instruction; the tail bound keeps a multi-MB transcript from eating the turns.
const updatePrompt = "Background update of a finished session (transcript: %s). " +
	"Read ONLY the last ~1500 lines of that transcript — it may be many MB and reading it whole will " +
	"exhaust your turn budget before you can write anything. " +
	"Then run `claude-code memory update` with no --transcript to print the narrative procedure, and follow it " +
	"COMPLETE, every step, not the abbreviated one. " +
	"Do it in this order so a truncated run still leaves the bank coherent: sessionHistory first, then " +
	"activeContext, then progress, decisionLog, and conventions. Be surgical; touch nothing else."

// historicalRule is appended to the worker prompt of a swept (older, finished) session.
const historicalRule = "\n\nThis is an OLDER session, not the current one: write sessionHistory.md and dated decisionLog.md " +
	"entries only, labelled with the session's real date. Do NOT edit activeContext.md, progress.md, conventions.md " +
	"or projectContext.md, and never write NOW or newest."

// currentStateFiles are the bank files an older session must never touch.
var currentStateFiles = []string{"activeContext.md", "progress.md", "conventions.md", "projectContext.md"}

// snapshotFiles reads each of names under dir; a missing file maps to nil.
func snapshotFiles(dir string, names []string) map[string][]byte {
	snap := make(map[string][]byte, len(names))
	for _, n := range names {
		snap[n], _ = os.ReadFile(filepath.Join(dir, n))
	}
	return snap
}

// warnChanged names every file whose bytes differ from snap; it never restores (a live session may own the write).
func warnChanged(out io.Writer, dir string, snap map[string][]byte) {
	for _, n := range slices.Sorted(maps.Keys(snap)) {
		if now, _ := os.ReadFile(filepath.Join(dir, n)); !bytes.Equal(now, snap[n]) {
			fmt.Fprintf(out, "memory update: WARNING: historical run changed %s (not restored)\n", n)
		}
	}
}

// Update statuses: pending = dispatched, not yet reported.
const (
	UpdatePending = "pending"
	UpdateOK      = "ok"
	UpdateFailed  = "failed"
)

// updateGrace must exceed the worker's own timeout, or a healthy long update reads as lost.
const updateGrace = 16 * time.Minute

// updateLockWait is how long an update waits for a peer's .postupdate lock before giving up as busy.
const updateLockWait = 5 * time.Second

// UpdateTimeout bounds the headless claude run.
const UpdateTimeout = 15 * time.Minute

// UpdateResult is one session's update outcome (KEY=value lines on disk).
type UpdateResult struct {
	Status      string
	Time        time.Time
	BankUpdated bool
	Log         string
}

func resultPath(sessionID string) string {
	return filepath.Join(StateDir(), "update-result-"+filepath.Base(sessionID))
}

// WriteUpdateResult persists r for sessionID; a blank id (manual run) records nothing.
func WriteUpdateResult(sessionID string, r UpdateResult) error {
	if sessionID == "" {
		return nil
	}
	body := "status=" + r.Status + "\ntime=" + r.Time.Format(time.RFC3339) +
		"\nbank_updated=" + strconv.FormatBool(r.BankUpdated) + "\nlog=" + r.Log + "\n"
	return paths.AtomicWrite(resultPath(sessionID), []byte(body), 0o600)
}

// ReadUpdateResult loads sessionID's result; ok=false when absent or unparseable.
func ReadUpdateResult(sessionID string) (UpdateResult, bool) {
	data, err := os.ReadFile(resultPath(sessionID))
	if err != nil {
		return UpdateResult{}, false
	}
	m := map[string]string{}
	for ln := range strings.SplitSeq(string(data), "\n") {
		if k, v, ok := strings.Cut(ln, "="); ok {
			m[k] = v
		}
	}
	t, err := time.Parse(time.RFC3339, m["time"])
	if err != nil {
		return UpdateResult{}, false
	}
	r := UpdateResult{Status: m["status"], Time: t, Log: m["log"], BankUpdated: m["bank_updated"] == "true"}
	return r, r.Status != ""
}

// NeedsAttention: a recorded failure, or a pending dispatch past the grace window (the worker died).
func (r UpdateResult) NeedsAttention(now time.Time) bool {
	return r.Status == UpdateFailed || (r.Status == UpdatePending && now.Sub(r.Time) > updateGrace)
}

// AgentFunc runs prompt on an injected agent (internal/agent imports this package) and names who answered.
type AgentFunc func(ctx context.Context, prompt, workdir string) (who string, err error)

// agentUpdatePrompt frames `claude-code memory update`'s procedure for an agent that has no slash
// commands; the procedure itself is one copy, embedded at prompts/update.md.
func agentUpdatePrompt(transcript, cwd string, historical bool) (string, error) {
	ref, err := Reference("update")
	if err != nil {
		return "", err
	}
	return "Background update of a finished session (transcript: " + transcript + "). " +
		"Read ONLY the last ~1500 lines of that transcript; it may be many MB. " +
		"Resolve the bank directory with `claude-code memory path`. Follow this procedure, the COMPLETE pass:\n\n" +
		ref + "\n\n" +
		"Write in this order so a truncated run leaves the bank coherent: sessionHistory first, then " +
		"activeContext, progress, decisionLog, conventions. Be surgical; touch nothing else." +
		gitSinceBlock(transcript, cwd) +
		tagBlock(transcript) + historicalBlock(historical), nil
}

func historicalBlock(historical bool) string {
	if historical {
		return historicalRule
	}
	return ""
}

// firstTranscriptTimestamp reads only the first JSONL line's "timestamp" field — the session's
// start time, used to scope `git log --since`. Zero time when absent or unparseable.
func firstTranscriptTimestamp(transcript string) time.Time {
	f, err := os.Open(transcript)
	if err != nil {
		return time.Time{}
	}
	defer f.Close()
	line, err := bufio.NewReader(f).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return time.Time{}
	}
	var rec struct {
		Timestamp string `json:"timestamp"`
	}
	if json.Unmarshal(line, &rec) != nil {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, rec.Timestamp)
	if err != nil {
		return time.Time{}
	}
	return t
}

// gitSinceBlock appends this session's commits and touched files (committed + working tree) so the
// narrative pass does not have to re-derive them. Empty when the transcript carries no timestamp or
// cwd is not a git repo — the pass still runs from the brief alone.
func gitSinceBlock(transcript, cwd string) string {
	since := firstTranscriptTimestamp(transcript)
	if since.IsZero() || cwd == "" {
		return ""
	}
	commits, _ := gitOut(cwd, "log", "--oneline", "--since="+since.Format(time.RFC3339))
	touched, _ := gitOut(cwd, "log", "--since="+since.Format(time.RFC3339), "--name-only", "--format=")
	status, _ := gitOut(cwd, "status", "--porcelain")
	if commits == "" && touched == "" && status == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nThis session's git activity (for the summary; verify before quoting):\n")
	if commits != "" {
		b.WriteString("Commits:\n")
		b.WriteString(commits)
		b.WriteString("\n")
	}
	if files := mergeFileLists(touched, status); len(files) > 0 {
		b.WriteString("Touched files:\n")
		b.WriteString(strings.Join(files, "\n"))
		b.WriteString("\n")
	}
	return b.String()
}

// mergeFileLists dedupes git log's --name-only paths with git status --porcelain's (status lines
// carry a 2-char code prefix; strip it before merging).
func mergeFileLists(nameOnly, porcelain string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if u, err := strconv.Unquote(p); strings.HasPrefix(p, `"`) && err == nil {
			p = u // git C-quotes odd names; Go's octal escapes are bytes, so UTF-8 survives
		}
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for ln := range strings.SplitSeq(nameOnly, "\n") {
		add(strings.TrimSpace(ln))
	}
	for ln := range strings.SplitSeq(porcelain, "\n") {
		ln = strings.TrimRight(ln, "\r") // the leading space is part of the "XY " code, so never trim it
		if len(ln) > 3 {
			_, p, renamed := strings.Cut(ln[3:], " -> ") // a rename prints "old -> new"; keep the new path
			if !renamed {
				p = ln[3:]
			}
			add(strings.TrimSpace(p))
		}
	}
	return out
}

// tagBlock extracts DECISION/BUG/NEXT/NOTE tags from the whole transcript and appends a filing
// instruction when any exist. Extraction failure (missing file, bad JSON) degrades to no block;
// the update itself must never fail because of tags.
func tagBlock(transcript string) string {
	f, err := os.Open(transcript)
	if err != nil {
		return ""
	}
	defer f.Close()
	tags, err := extractTags(f)
	if err != nil || len(tags) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nTagged this session — file every one: DECISION → decisionLog (dated entry), " +
		"BUG → progress known issues, NEXT → activeContext next steps, NOTE → conventions or progress; " +
		"skip any already filed.\n")
	for _, t := range tags {
		b.WriteString("- ")
		b.WriteString(t.Kind)
		b.WriteString(": ")
		b.WriteString(t.Text)
		b.WriteString("\n")
	}
	return b.String()
}

// updateOptions drive one background update.
type updateOptions struct {
	Claude     string    // real claude binary; "" records a failure unless Run is set
	Run        AgentFunc // preferred agent; nil = claude -p
	Transcript string
	CWD        string
	Session    string
	Historical bool          // an older session: the prompt forbids current-state files and a changed one is reported
	Timeout    time.Duration // 0 = UpdateTimeout
	Out        io.Writer
}

// update runs the headless narrative pass under the bank lock, settles the bank, and records the outcome.
func update(o updateOptions) error {
	o = updateDefaults(o)
	prior, _ := ReadUpdateResult(o.Session)
	record := func(status string, updated bool) error {
		err := WriteUpdateResult(o.Session, UpdateResult{Status: status, Time: time.Now(), BankUpdated: updated, Log: prior.Log})
		if err != nil {
			return fmt.Errorf("record update result: %w", err)
		}
		return nil
	}
	if o.Claude == "" && o.Run == nil {
		return errors.Join(errors.New("claude binary not found"), record(UpdateFailed, false))
	}
	dir := Dir(o.CWD)
	var run updateRun
	// Bounded wait: a peer update/dream may hold this lock for minutes, and a Stop hook must not block on it.
	ran, lockErr := paths.TryFileLock(lockPath(dir)+".lock", updateLockWait, func() error { return run.locked(o, dir) })
	if !ran { // lock never taken: nothing ran, and settling unlocked would race the holder
		return errors.Join(fmt.Errorf("headless update: %w", cmp.Or(lockErr, errors.New("another update or dream holds the store lock"))), record(UpdateFailed, false))
	}
	if err := errors.Join(lockErr, run.runErr, run.settleErr); err != nil {
		return errors.Join(fmt.Errorf("headless update: %w", err), record(UpdateFailed, run.updated))
	}
	return record(UpdateOK, run.updated)
}

func updateDefaults(o updateOptions) updateOptions {
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Timeout == 0 {
		o.Timeout = UpdateTimeout
	}
	return o
}

// updateRun is what the locked section of Update observed: bank mtimes before the run, the run's error, and settlement.
type updateRun struct {
	before, beforeHome time.Time
	runErr, settleErr  error
	updated            bool
}

// finish settles under the writer lock and reports whether the update wrote either bank tier.
func (r *updateRun) finish(o updateOptions, dir string) (updated bool, err error) {
	// An update may legitimately write only the shared home bank (cross-repo facts), so watch both tiers.
	updated = NewestMtime(dir).After(r.before) || NewestMtime(HomeBankDir()).After(r.beforeHome)
	if _, err := settleBank(o.CWD, time.Now()); err != nil {
		return updated, fmt.Errorf("settle: %w", err)
	}
	// Refresh the cached doctor notice here (background worker), never on the SessionStart launch path.
	if err := writeDoctorNotice(o.CWD, DefaultStaleDays, time.Now()); err != nil {
		fmt.Fprintln(o.Out, "memory update: doctor notice:", err)
	}
	return updated, nil
}

// locked runs the agent or claude update and its settlement under the store lock; errors ride in the run struct.
func (r *updateRun) locked(o updateOptions, dir string) error {
	if o.Historical {
		snap := snapshotFiles(dir, currentStateFiles)
		defer warnChanged(o.Out, dir, snap)
	}
	r.before, r.beforeHome = NewestMtime(dir), NewestMtime(HomeBankDir())
	ctx, cancel := context.WithTimeout(context.Background(), o.Timeout)
	defer cancel()
	if o.Run != nil {
		r.runErr = runAgentUpdate(ctx, o)
	} else {
		r.runErr = runClaudeUpdate(ctx, o)
	}
	r.updated, r.settleErr = r.finish(o, dir)
	return nil
}

// runAgentUpdate hands the update prompt to o.Run and reports which agent ran it.
func runAgentUpdate(ctx context.Context, o updateOptions) error {
	prompt, err := agentUpdatePrompt(o.Transcript, o.CWD, o.Historical)
	if err != nil {
		return err
	}
	_ = os.Setenv(UpdateChildEnv, "1") // a Claude fallback dispatch must not re-trigger hooks; cannot fail for a fixed key
	who, err := o.Run(ctx, prompt, o.CWD)
	fmt.Fprintln(o.Out, "memory update: ran on", who)
	return err
}

// headlessClaudeEnv marks an update child; a stored setup-token keeps it working after the interactive /login expires.
func headlessClaudeEnv() []string {
	env := append(os.Environ(), UpdateChildEnv+"=1")
	if os.Getenv(providers.OAuthTokenEnv) == "" {
		if tok := providers.HeadlessOAuthToken(); tok != "" {
			env = append(env, providers.OAuthTokenEnv+"="+tok)
		}
	}
	return env
}

// runClaudeUpdate runs `claude -p` with the update prompt, streaming to o.Out.
func runClaudeUpdate(ctx context.Context, o updateOptions) error {
	cmd := exec.CommandContext(ctx, o.Claude, "-p", fmt.Sprintf(updatePrompt, o.Transcript)+tagBlock(o.Transcript)+historicalBlock(o.Historical), "--max-turns", "40")
	cmd.Dir = o.CWD
	cmd.Env = headlessClaudeEnv()
	cmd.Stdout, cmd.Stderr = o.Out, o.Out
	return cmd.Run()
}

// UpdateChildEnv marks a spawned update worker so its own session hooks never schedule another.
const UpdateChildEnv = "CLAUDE_SESSION_UPDATE_CHILD"

// IsUpdateChild reports whether this process runs inside an update worker.
func IsUpdateChild() bool {
	v := os.Getenv(UpdateChildEnv)
	return v != "" && v != "0"
}

// tag is one DECISION/BUG/NEXT/NOTE line the assistant wrote in a reply.
type tag struct {
	Kind string
	Text string
}

// tagMaxLen caps a single tag; tagMaxCount keeps only the newest tags (dedup preserves first-seen order).
const (
	tagMaxLen   = 300
	tagMaxCount = 40
)

var tagKinds = []string{"DECISION:", "BUG:", "NEXT:", "NOTE:"}

// extractTags streams transcript JSONL and returns every DECISION/BUG/NEXT/NOTE line from ASSISTANT
// text blocks only — user, tool_result and system content never contribute a tag.
func extractTags(r io.Reader) ([]tag, error) {
	var tags []tag
	seen := map[string]bool{}
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			for _, t := range tagsFromLine(line) {
				key := t.Kind + "\x00" + t.Text
				if !seen[key] {
					seen[key] = true
					tags = append(tags, t)
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
	}
	if len(tags) > tagMaxCount {
		tags = tags[len(tags)-tagMaxCount:]
	}
	return tags, nil
}

// tagsFromLine parses one JSONL record and extracts tags from its assistant text blocks, if any.
func tagsFromLine(line []byte) []tag {
	var rec struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(bytes.TrimSpace(line), &rec) != nil {
		return nil
	}
	if rec.Type != "assistant" || rec.Message.Role != "assistant" {
		return nil
	}
	var tags []tag
	for _, block := range rec.Message.Content {
		if block.Type != "text" {
			continue
		}
		for ln := range strings.SplitSeq(block.Text, "\n") {
			if t, ok := parseTagLine(ln); ok {
				tags = append(tags, t)
			}
		}
	}
	return tags
}

// parseTagLine matches an optional "- "/"* " bullet then a DECISION:/BUG:/NEXT:/NOTE: prefix.
func parseTagLine(ln string) (tag, bool) {
	ln = strings.TrimSpace(ln)
	ln = strings.TrimPrefix(strings.TrimPrefix(ln, "- "), "* ")
	for _, kind := range tagKinds {
		if after, ok := strings.CutPrefix(ln, kind); ok {
			text := strings.TrimSpace(after)
			if len(text) > tagMaxLen {
				text = strings.ToValidUTF8(text[:tagMaxLen], "") // a cut mid-rune leaves a partial one
			}
			return tag{Kind: strings.TrimSuffix(kind, ":"), Text: text}, true
		}
	}
	return tag{}, false
}
