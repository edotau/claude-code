// Package agent runs one headless task on an agent runner (claude, api, codex, gemini, opencode, copilot)
// under one retry loop; every runner returns the same Result envelope.
package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Result is the envelope `ask --format json` prints.
type Result struct {
	Agent     string        `json:"agent"`
	Provider  string        `json:"provider,omitempty"`
	Model     string        `json:"model,omitempty"`
	SessionID string        `json:"session_id,omitempty"`
	Answer    string        `json:"answer"`
	Usage     Usage         `json:"usage"`
	Duration  time.Duration `json:"duration_ns"`
	Dropped   []string      `json:"dropped,omitempty"`
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
		return Classify(r.Name(), err, cw.n > 0)
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

func errPromptTooLarge(n int) error {
	return fmt.Errorf("prompt is %d bytes, over the %d limit for one argv element", n, MaxPromptBytes)
}
