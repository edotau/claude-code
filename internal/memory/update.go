package memory

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// UpdatePrompt is the headless update instruction; the tail bound keeps a multi-MB transcript from eating the turns.
const UpdatePrompt = "Background update of a finished session (transcript: %s). " +
	"Read ONLY the last ~1500 lines of that transcript — it may be many MB and reading it whole will " +
	"exhaust your turn budget before you can write anything. " +
	"Then run `claude-code memory update` with no --transcript to print the narrative procedure, and follow it " +
	"COMPLETE, every step, not the abbreviated one. " +
	"Do it in this order so a truncated run still leaves the bank coherent: sessionHistory first, then " +
	"activeContext, then progress, decisionLog, and conventions. Be surgical; touch nothing else."

// Update statuses: pending = dispatched, not yet reported.
const (
	UpdatePending = "pending"
	UpdateOK      = "ok"
	UpdateFailed  = "failed"
)

// updateGrace must exceed the worker's own timeout, or a healthy long update reads as lost.
const updateGrace = 16 * time.Minute

// UpdateTimeout bounds the headless claude run.
const UpdateTimeout = 15 * time.Minute

// StateDir holds update stamps, results, logs and the session chain.
func StateDir() string { return filepath.Join(paths.StateDir(), "memory") }

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
	r := UpdateResult{Status: m["status"], Log: m["log"], BankUpdated: m["bank_updated"] == "true"}
	r.Time, _ = time.Parse(time.RFC3339, m["time"])
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
func agentUpdatePrompt(transcript, cwd string) (string, error) {
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
		tagBlock(transcript), nil
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
		b.WriteString("Commits:\n" + commits + "\n")
	}
	if files := mergeFileLists(touched, status); len(files) > 0 {
		b.WriteString("Touched files:\n" + strings.Join(files, "\n") + "\n")
	}
	return b.String()
}

// mergeFileLists dedupes git log's --name-only paths with git status --porcelain's (status lines
// carry a 2-char code prefix; strip it before merging).
func mergeFileLists(nameOnly, porcelain string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for ln := range strings.SplitSeq(nameOnly, "\n") {
		add(strings.TrimSpace(ln))
	}
	for ln := range strings.SplitSeq(porcelain, "\n") {
		ln = strings.TrimSpace(ln)
		if len(ln) > 3 {
			add(strings.TrimSpace(ln[3:]))
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
	tags, err := ExtractTags(f)
	if err != nil || len(tags) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nTagged this session — file every one: DECISION → decisionLog (dated entry), " +
		"BUG → progress known issues, NEXT → activeContext next steps, NOTE → conventions or progress; " +
		"skip any already filed.\n")
	for _, t := range tags {
		b.WriteString("- " + t.Kind + ": " + t.Text + "\n")
	}
	return b.String()
}

// UpdateOptions drive one background update.
type UpdateOptions struct {
	Claude     string    // real claude binary; "" records a failure unless Run is set
	Run        AgentFunc // preferred runner; nil = claude -p
	Transcript string
	CWD        string
	Session    string
	Timeout    time.Duration // 0 = UpdateTimeout
	Out        io.Writer
}

// Update runs the headless narrative pass under the bank lock, settles the bank, and records the outcome.
func Update(o UpdateOptions) error {
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Timeout == 0 {
		o.Timeout = UpdateTimeout
	}
	prior, _ := ReadUpdateResult(o.Session)
	record := func(status string, updated bool) {
		_ = WriteUpdateResult(o.Session, UpdateResult{Status: status, Time: time.Now(), BankUpdated: updated, Log: prior.Log})
	}
	if o.Claude == "" && o.Run == nil {
		record(UpdateFailed, false)
		return errors.New("claude binary not found")
	}
	dir := Dir(o.CWD)
	var before, beforeHome time.Time
	var runErr error
	lockErr := paths.WithFileLock(lockPath(dir), func() error {
		before, beforeHome = NewestMtime(dir), NewestMtime(HomeBankDir())
		ctx, cancel := context.WithTimeout(context.Background(), o.Timeout)
		defer cancel()
		if o.Run != nil {
			prompt, perr := agentUpdatePrompt(o.Transcript, o.CWD)
			if perr != nil {
				runErr = perr
				return nil
			}
			_ = os.Setenv(UpdateChildEnv, "1") // a Claude fallback dispatch must not re-trigger hooks; cannot fail for a fixed key
			who, err := o.Run(ctx, prompt, o.CWD)
			runErr = err
			fmt.Fprintln(o.Out, "memory update: ran on", who)
			return nil
		}
		cmd := exec.CommandContext(ctx, o.Claude, "-p", fmt.Sprintf(UpdatePrompt, o.Transcript)+tagBlock(o.Transcript), "--max-turns", "40")
		cmd.Dir = o.CWD
		cmd.Env = append(os.Environ(), UpdateChildEnv+"=1")
		// A stored setup-token keeps the unattended update working after the interactive /login session expires.
		if os.Getenv(providers.OAuthTokenEnv) == "" {
			if tok := providers.HeadlessOAuthToken(); tok != "" {
				cmd.Env = append(cmd.Env, providers.OAuthTokenEnv+"="+tok)
			}
		}
		cmd.Stdout, cmd.Stderr = o.Out, o.Out
		runErr = cmd.Run()
		return nil
	})
	// An update may legitimately write only the shared home bank (cross-repo facts), so watch both tiers.
	updated := NewestMtime(dir).After(before) || NewestMtime(HomeBankDir()).After(beforeHome)
	if _, err := SettleBank(o.CWD, time.Now()); err != nil {
		fmt.Fprintln(o.Out, "memory update: settle:", err)
	}
	// Refresh the cached doctor notice here (background worker), never on the SessionStart launch path.
	if err := WriteDoctorNotice(o.CWD, DefaultStaleDays, time.Now()); err != nil {
		fmt.Fprintln(o.Out, "memory update: doctor notice:", err)
	}
	if err := errors.Join(lockErr, runErr); err != nil {
		record(UpdateFailed, updated)
		return fmt.Errorf("headless update: %w", err)
	}
	record(UpdateOK, updated)
	return nil
}
