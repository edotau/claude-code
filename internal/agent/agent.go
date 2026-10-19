// Package agent runs one headless task on an agent runner (claude, api, codex, gemini, opencode, copilot)
// under one retry loop; every runner returns the same Result envelope.
package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

// Request is one task. Model may be a slot name, a model id, or provider:model.
type Request struct {
	Prompt, Model, Provider, Effort, WorkDir, SessionID string
	Tools                                               bool
	Timeout                                             time.Duration
}

// Usage is token accounting where the runner reports it.
type Usage struct {
	InputTokens         int `json:"input_tokens"`                    // uncached input only
	CacheCreationTokens int `json:"cache_creation_tokens,omitempty"` // billed above input; kept apart so cost can be priced
	CacheReadTokens     int `json:"cache_read_tokens,omitempty"`
	OutputTokens        int `json:"output_tokens"`
}

// Result is the envelope `ask --format json` prints.
type Result struct {
	Agent          string        `json:"agent"`
	Provider       string        `json:"provider,omitempty"`
	Model          string        `json:"model,omitempty"`
	SessionID      string        `json:"session_id,omitempty"`
	Answer         string        `json:"answer"`
	Usage          Usage         `json:"usage"`
	Duration       time.Duration `json:"duration_ns"`
	Dropped        []string      `json:"dropped,omitempty"`
	Fallback       string        `json:"fallback,omitempty"`        // set to the original runner's name when it ran instead
	FallbackReason string        `json:"fallback_reason,omitempty"` // why the original runner was unavailable
}

// Caps declares which Request fields a runner honors; Prompt and Timeout are always honored.
type Caps struct{ Model, Provider, Effort, WorkDir, SessionID, Tools bool }

// Runner is one agent backend.
type Runner interface {
	Name() string
	Caps() Caps
	Available() error
	Run(ctx context.Context, req Request, stream io.Writer) (Result, error)
}

// Runners lists every runner in roster order.
func Runners() []Runner {
	return []Runner{claudeRunner{}, apiRunner{}, vendorRunner{"codex"}, vendorRunner{"gemini"}, vendorRunner{"opencode"}, copilotRunner{}}
}

// Lookup finds a runner by name.
func Lookup(name string) (Runner, error) {
	var names []string
	for _, r := range Runners() {
		if r.Name() == name {
			return r, nil
		}
		names = append(names, r.Name())
	}
	return nil, fmt.Errorf("unknown agent %q (%s)", name, strings.Join(names, "|"))
}

// applyCaps clears fields the runner cannot honor and names each one, so a drop is never silent.
func applyCaps(c Caps, req Request) (Request, []string) {
	var dropped []string
	drop := func(ok bool, set bool, flag string, clear func()) {
		if set && !ok {
			dropped = append(dropped, flag)
			clear()
		}
	}
	drop(c.Model, req.Model != "", "--model="+req.Model, func() { req.Model = "" })
	drop(c.Provider, req.Provider != "", "--provider="+req.Provider, func() { req.Provider = "" })
	drop(c.Effort, req.Effort != "", "--effort="+req.Effort, func() { req.Effort = "" })
	drop(c.WorkDir, req.WorkDir != "", "--workdir="+req.WorkDir, func() { req.WorkDir = "" })
	drop(c.SessionID, req.SessionID != "", "--session="+req.SessionID, func() { req.SessionID = "" })
	drop(c.Tools, req.Tools, "--tools", func() { req.Tools = false })
	return req, dropped
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// Run drops unsupported fields loudly, then runs r under DefaultPolicy with req.Timeout per attempt.
func Run(ctx context.Context, r Runner, req Request, stream io.Writer, notify func(string)) (Result, error) {
	req, dropped := applyCaps(r.Caps(), req)
	res := Result{Agent: r.Name(), Dropped: dropped}
	if err := r.Available(); err != nil {
		return res, &Error{Kind: KindFatal, Agent: r.Name(), Err: err}
	}
	p := DefaultPolicy()
	if req.Timeout > 0 {
		p.AttemptTimeout = req.Timeout
	}
	p.Notify = notify
	if _, t, err := resolveTarget(req); err == nil && t.Provider.Auth.Command != "" {
		p.Recover = func(ctx context.Context) error { _, err := providers.Refresh(ctx, t.Provider); return err }
	}
	start := time.Now()
	e := Do(ctx, p, func(actx context.Context, _ int) *Error {
		cw := &countingWriter{w: stream}
		out, err := r.Run(actx, req, cw)
		out.Agent, out.Dropped = res.Agent, res.Dropped
		res = out
		e := Classify(r.Name(), err, cw.n > 0)
		// A hard-quota 429 means this key can never use that model: fall to the next slot instead of waiting it out.
		if e != nil && e.Kind == KindRateLimit && providers.HardQuota(e.Err.Error()) && r.Caps().Model &&
			(req.Model == "" || slices.Contains(models.Slots, req.Model)) {
			cur := cmp.Or(req.Model, models.Opus)
			if next := models.NextSlot(cur); next != "" {
				if notify != nil {
					notify(fmt.Sprintf("%s: the %s slot is quota-dead on this key; retrying on the %s slot", r.Name(), cur, next))
				}
				req.Model, e.RetryAfter = next, 0
			}
		}
		// A tools-on run that timed out may already have edited files: re-running it would be a second writer.
		if e != nil && req.Tools && actx.Err() == context.DeadlineExceeded {
			e.Kind = KindFatal
			e.Err = fmt.Errorf("%w (tools were on, so the attempt is not retried: it may have changed files; inspect and re-run by hand)", e.Err)
		}
		return e
	})
	res.Duration = time.Since(start)
	if e != nil {
		return res, e
	}
	return res, nil
}

// resolveTarget applies --provider then --model (slot name, id, or provider:model) to the registry.
func resolveTarget(req Request) (*providers.Registry, providers.Target, error) {
	reg, err := providers.Load()
	if err != nil {
		return nil, providers.Target{}, err
	}
	sel, err := providers.Select(reg, req.Provider)
	if sel.Provider == nil || (err != nil && req.Model == "") {
		return nil, providers.Target{}, err
	}
	if req.Model == "" {
		return reg, sel.Slots[models.Opus], nil
	}
	if t, ok := sel.Slots[req.Model]; ok {
		return reg, t, nil
	}
	return reg, providers.ParseTarget(reg, req.Model, sel.Provider), nil
}

// MaxPromptBytes keeps a prompt passed as one argv element under the kernel's 128 KiB MAX_ARG_STRLEN.
const MaxPromptBytes = 96 << 10

// RubricPrompt frames task against the review standard in file, quoted verbatim.
func RubricPrompt(file, task string) (string, error) {
	standard, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("rubric: %w", err)
	}
	prompt := "You are reviewing against a fixed standard, quoted in full below.\n\n" +
		"=== BEGIN REVIEW STANDARD (the rubric, NOT the code under review) ===\n" + string(standard) +
		"\n=== END REVIEW STANDARD ===\n\nTASK: " + task + "\n\nRules for your output:\n" +
		"- Apply the standard exactly as written, including every exemption it states.\n" +
		"- Report only violations you can point to, with file and approximate line, quoting the code.\n" +
		"- Do not report on the standard's own text or examples.\n" +
		"- If nothing violates the standard, say exactly: no findings.\n"
	if len(prompt) > MaxPromptBytes {
		return "", fmt.Errorf("rubric: composed prompt is %d bytes, over the %d limit", len(prompt), MaxPromptBytes)
	}
	return prompt, nil
}

// childCmd builds an exec.Cmd in its own process group; a deadline kills the whole tree.
func childCmd(ctx context.Context, bin string, argv, env []string, dir, stdin string) (*exec.Cmd, *TailBuffer) {
	cmd := exec.CommandContext(ctx, bin, argv...)
	ownGroup(cmd)
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = append(os.Environ(), env...) // later duplicates win
	cmd.Dir = dir
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	tail := &TailBuffer{}
	cmd.Stderr = tail
	return cmd, tail
}

// childErr names a failed child by its stderr tail; a deadline surfaces as ctx.Err for classification.
func childErr(ctx context.Context, name string, err error, tail *TailBuffer) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%s: %w", name, ctx.Err())
	}
	if t := strings.TrimSpace(tail.String()); t != "" {
		if len(t) > 600 {
			t = "…" + t[len(t)-600:]
		}
		return fmt.Errorf("%s: %w: %s", name, err, t)
	}
	return fmt.Errorf("%s: %w", name, err)
}

// ErrPromptTooLarge is the sentinel behind errPromptTooLarge, so callers can exclude it with errors.Is.
var ErrPromptTooLarge = errors.New("prompt too large")

func errPromptTooLarge(n int) error {
	return fmt.Errorf("%w: %d bytes, over the %d limit for one argv element", ErrPromptTooLarge, n, MaxPromptBytes)
}

// Fallback modes for an unavailable vendor leg: the full chain ending in claude, other vendors only, or none.
const (
	FallbackAll     = "all"
	FallbackVendors = "vendors"
	FallbackNone    = "none"
)

// FallbackModes lists the accepted --fallback values.
var FallbackModes = []string{FallbackAll, FallbackVendors, FallbackNone}

// FallbackChain is what an unavailable r falls back to: the other installed vendor runners in roster order
// (a second model family stays a second family), then claude last under FallbackAll. claude/api never fall back.
func FallbackChain(r Runner, mode string) []Runner {
	if mode == FallbackNone || !isVendor(r.Name()) {
		return nil
	}
	var chain []Runner
	for _, c := range Runners() {
		if !isVendor(c.Name()) || c.Name() == r.Name() || c.Available() != nil {
			continue
		}
		// codex and gemini: first party, then Copilot on the same model, then claude (user decision) — no other vendor.
		if copilotOnly[r.Name()] && c.Name() != "copilot" {
			continue
		}
		chain = append(chain, c)
	}
	if mode == FallbackAll {
		chain = append(chain, claudeRunner{})
	}
	return chain
}

// copilotOnly names the first-party legs whose only vendor stand-in is Copilot.
var copilotOnly = map[string]bool{"codex": true, "gemini": true}

// sameModelOnCopilot names the legs whose model ids Copilot also serves (GPT); Copilot has no Gemini models.
var sameModelOnCopilot = map[string]bool{"codex": true}

func isVendor(name string) bool { return name != "claude" && name != "api" }

// RunWithFallback runs r, then each leg of chain in turn while the previous one turned out unavailable — binary
// missing, no provider can serve it, a login/quota failure, or an exit with no answer. It never moves on after a
// timeout, cancellation, an oversized prompt or a partial answer, nor when the caller pinned an explicit
// non-Claude --provider/--model. The result names the original runner in Fallback and every skipped leg's reason.
func RunWithFallback(ctx context.Context, r Runner, chain []Runner, req Request, stream io.Writer, notify func(string)) (Result, error) {
	res, err := Run(ctx, r, req, stream, notify)
	if explicitNonClaude(req) {
		return res, err
	}
	var reasons []string
	prev := r
	for _, fb := range chain {
		if !legUnavailable(prev, res, err) {
			break
		}
		reasons = append(reasons, legLabel(prev)+": "+fallbackReason(err))
		if notify != nil {
			notify(fmt.Sprintf("%s unavailable (%s) — falling back to %s", legLabel(prev), fallbackReason(err), legLabel(fb)))
		}
		res, err = Run(ctx, fb, fallbackRequest(r, fb, req, res.Model), stream, notify)
		prev = fb
	}
	if len(reasons) > 0 {
		res.Fallback, res.FallbackReason = r.Name(), strings.Join(reasons, "; ")
	}
	return res, err
}

// legLabel names a leg in fallback reasons: a hub leg by its account, since every hub leg shares the agent name.
func legLabel(r Runner) string {
	if l, ok := r.(hubLeg); ok {
		return l.provider.Name
	}
	return r.Name()
}

// fallbackRequest drops the original leg's provider and vendor model: codex→copilot keeps the model codex ran
// (prevModel), other vendor legs take their own default, and the claude leg is a sonnet leg unless the caller
// asked for a Claude slot or id (unset would mean opus).
func fallbackRequest(from, fb Runner, req Request, prevModel string) Request {
	out := req
	out.Provider, out.SessionID = "", "" // a session id belongs to the leg that minted it
	if fb.Name() != "claude" {
		out.Model = ""
		if fb.Name() == "copilot" && sameModelOnCopilot[from.Name()] {
			out.Model = models.Strip1M(cmp.Or(prevModel, req.Model)) // the caller's model when codex never resolved one
		}
		return out
	}
	if out.Model == "" {
		out.Model = models.Sonnet
	} else if !slices.Contains(models.Slots, out.Model) {
		if _, _, _, ok := models.ParseClaudeID(out.Model); !ok {
			out.Model = models.Sonnet
		}
	}
	return out
}

// fallbackReason keeps the last line of the failure (a vendor CLI prints its own ERROR line last), capped at 200 bytes.
func fallbackReason(err error) string {
	lines := strings.Split(strings.TrimSpace(err.Error()), "\n")
	s := strings.TrimSpace(lines[len(lines)-1])
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// legUnavailable narrows "the leg failed" to "the leg itself is unreachable": the binary, the resolved target,
// or the exec failed before producing an answer — excluding timeouts, cancellation, an oversized prompt and a
// partial answer. claude/api are the end of every chain and never count as unavailable here.
func legUnavailable(r Runner, res Result, err error) bool {
	if err == nil || !isVendor(r.Name()) || res.Answer != "" {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, ErrPromptTooLarge) {
		return false
	}
	var e *Error
	return !(errors.As(err, &e) && e.Progressed)
}

// explicitNonClaude is true when --provider, or a "provider:model" --model, names a provider outright;
// the fallback must never override a caller's explicit choice of backend.
func explicitNonClaude(req Request) bool {
	if req.Provider != "" {
		return true
	}
	reg, _ := providers.Load()
	return namesProvider(reg, req.Model)
}
