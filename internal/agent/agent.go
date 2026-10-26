// Package agent runs one headless task on an agent runner (claude, api, codex, gemini, opencode, copilot)
// under one retry loop; every runner returns the same Result envelope.
package agent

import (
	"bytes"
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
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/envscrub"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

// Request is one task. Model may be a slot name, a model id, or provider:model.
type Request struct {
	Prompt, Model, Provider, Effort, WorkDir, SessionID string
	Attach                                              []string // absolute file paths, task text precedes them
	KeepEnv                                             []string // names the credential scrub must not drop, from --keep-env
	Tools, Autonomous, EnableMemory, Direct             bool     // Direct: vendor keeps the provider route, skips the router
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

// Caps declares which Request fields a runner honors; Prompt, Timeout and Attach are always honored
// (every runner has at least an inline-block fallback for Attach).
type Caps struct{ Model, Provider, Effort, WorkDir, SessionID, Tools, Autonomous, Memory bool }

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
	drop(c.Autonomous, req.Autonomous, "--autonomous", func() { req.Autonomous = false })
	drop(c.Memory, req.EnableMemory, "--enable-memory", func() { req.EnableMemory = false })
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
	if stream == nil {
		stream = io.Discard
	}
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
	var recoverErr error
	if _, t, err := resolveTarget(req); err == nil && (t.Provider.Auth.Command != "" || t.Provider.Auth.AuthType != "") {
		p.Recover = func(ctx context.Context) error {
			_, recoverErr = providers.Refresh(ctx, t.Provider)
			return recoverErr
		}
	}
	start := time.Now()
	e := Do(ctx, p, func(actx context.Context, _ int) *Error {
		cw := &countingWriter{w: stream}
		out, err := r.Run(actx, req, cw)
		// A runner that delegated (e.g. api on a passthrough provider) names the actual runner itself;
		// only default to r.Name() when it left Agent unset, so the envelope doesn't lie about who ran.
		out.Agent, out.Dropped = cmp.Or(out.Agent, res.Agent), res.Dropped
		res = out
		e := Classify(r.Name(), err, cw.n > 0)
		// A hard-quota 429 means this key can never use that model: fall to the next slot instead of waiting it out.
		if e != nil && (e.Kind == KindRateLimit || e.Kind == KindQuota) && providers.ModelDenied(e.Err.Error()) &&
			r.Caps().Model && (req.Model == "" || slices.Contains(models.Slots, req.Model)) {
			cur := cmp.Or(req.Model, models.Opus)
			if next := models.NextSlot(cur); next != "" {
				if notify != nil {
					notify(fmt.Sprintf("%s: the %s slot is quota-dead on this key; retrying on the %s slot", r.Name(), cur, next))
				}
				req.Model, e.RetryAfter, e.Kind = next, 0, KindRateLimit
			}
		}
		return e
	})
	res.Duration = time.Since(start)
	if e != nil {
		condemnAuth(e, recoverErr)
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

// attachSuffix appends each path as "@<path>" after prompt: copilot and agy resolve it live, at turn
// time, from the real file — task text first, since agy truncates the tail.
func attachSuffix(prompt string, paths []string) string {
	var b strings.Builder
	b.WriteString(prompt)
	for _, p := range paths {
		fmt.Fprintf(&b, "\n@%s", p)
	}
	return b.String()
}

// attachInline embeds each path as a literal <file path="…">content</file> block after prompt: the
// fallback for a runner with no live-read channel (codex's prompt rides stdin blind).
func attachInline(prompt string, paths []string) (string, error) {
	if len(paths) == 0 {
		return prompt, nil
	}
	var b strings.Builder
	b.WriteString(prompt)
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("attach %s: %w", p, err)
		}
		if !IsText(data) {
			return "", fmt.Errorf("--attach %s: binary file; attach text only", p)
		}
		fmt.Fprintf(&b, "\n\n<file path=%q>\n%s\n</file>", p, data)
	}
	return b.String(), nil
}

// IsText reports whether data can ride inline in a prompt: valid UTF-8 with no NUL byte.
func IsText(data []byte) bool { return bytes.IndexByte(data, 0) < 0 && utf8.Valid(data) }

// childCmd builds an exec.Cmd in its own process group; a deadline kills the whole tree. The parent env
// is scrubbed of inherited credentials before env (the launch's own additions) is layered on. vendor
// also scrubs HARNESS_VAULT_KEY: true for every vendor CLI child, false for the claude runner's own child.
func childCmd(ctx context.Context, bin string, argv, env []string, dir, stdin string, keepEnv []string, vendor bool) (*exec.Cmd, *TailBuffer) {
	cmd := exec.CommandContext(ctx, bin, argv...)
	ownGroup(cmd)
	cmd.WaitDelay = 5 * time.Second
	scrub := envscrub.Scrub
	if vendor {
		scrub = envscrub.ScrubVendor
	}
	base, _ := scrub(os.Environ(), envscrub.KeepSet(keepEnv))
	cmd.Env = append(base, env...) // later duplicates win
	cmd.Dir = dir
	if dir != "" {
		cmd.Env = append(cmd.Env, "PWD="+dir) // a child that reads $PWD (not getcwd()) still sees dir
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	tail := &TailBuffer{}
	cmd.Stderr = io.Writer(tail) // the dispatch log, when ctx carries one, sees stderr as it arrives
	if sk := stderrLogFrom(ctx); sk != nil {
		cmd.Stderr = io.MultiWriter(tail, sk.Stream())
	}
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

func isVendor(name string) bool { return name != "claude" && name != "api" }

// RunWithFallback runs c's legs in turn while the previous one turned out unavailable — binary missing, no provider
// can serve it, a login/quota failure, or an exit with no answer. It never moves on after a timeout, cancellation,
// an oversized prompt or a partial answer, nor when the caller pinned an explicit non-Claude --provider/--model.
// A KindQuota failure marks the leg's account until its reset. The result names the wanted runner in Fallback and
// every gate skip and failed leg in FallbackReason.
func RunWithFallback(ctx context.Context, c Chain, req Request, stream io.Writer, notify func(string)) (Result, error) {
	reasons := slices.Clone(c.Skipped)
	if len(c.Legs) == 0 {
		return Result{Agent: c.Want, FallbackReason: strings.Join(reasons, "; ")},
			&Error{Kind: KindFatal, Agent: c.Want, Err: fmt.Errorf("no runner available: %s", strings.Join(reasons, "; "))}
	}
	if len(reasons) > 0 && notify != nil {
		notify("skipped without an attempt — " + strings.Join(reasons, "; "))
	}
	var res Result
	var err error
	var prev Runner
	for i, leg := range c.Legs {
		if i > 0 {
			if !legUnavailable(prev, res, err) {
				break
			}
			reasons = append(reasons, legLabel(prev)+": "+fallbackReason(err))
			if notify != nil {
				notify(fmt.Sprintf("%s unavailable (%s) — falling back to %s%s", legLabel(prev), fallbackReason(err), legLabel(leg.Runner), legCell(leg)))
			}
		}
		res, err = Run(ctx, leg.Runner, legRequest(req, leg, i == 0 && leg.Runner.Name() == c.Want), stream, notify)
		noteQuota(leg, err)
		noteAuth(leg, err)
		noteStall(leg, err)
		prev = leg.Runner
		if i == 0 && ExplicitBackend(req) {
			break
		}
	}
	if len(reasons) > 0 || c.Legs[0].Runner.Name() != c.Want {
		res.Fallback, res.FallbackReason = c.Want, strings.Join(reasons, "; ")
	}
	return res, err
}

// legCell is " (model, effort)" for a routed leg, "" for an unrouted one.
func legCell(l Leg) string {
	if l.Model == "" {
		return ""
	}
	if l.Effort != "" {
		return " (" + l.Model + ", " + l.Effort + ")"
	}
	return " (" + l.Model + ")"
}

// legLabel names a leg in fallback reasons: a hub leg by its account, since every hub leg shares the agent name.
func legLabel(r Runner) string {
	if l, ok := r.(hubLeg); ok {
		return l.provider.Name
	}
	return r.Name()
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
	var ae *Error
	if errors.As(err, &ae) && ae.AuthReason != "" && res.Answer == "" {
		return true
	}
	if err == nil || !isVendor(r.Name()) || res.Answer != "" {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, ErrPromptTooLarge) {
		return false
	}
	var e *Error
	return !errors.As(err, &e) || !e.Progressed
}

// ExplicitBackend is true when --provider, or a "provider:model" --model, names a provider outright;
// the fallback must never override a caller's explicit choice of backend.
func ExplicitBackend(req Request) bool {
	if req.Provider != "" {
		return true
	}
	reg, _ := providers.Load()
	return namesProvider(reg, req.Model)
}
