// Package agent runs one headless task on an agent (claude, codex, gemini, opencode, copilot)
// under one retry loop; every agent returns the same Result envelope.
package agent

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/envscrub"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets/shapes"
)

// Request is one task. Model may be a model alias, a model id, or provider:model.
type Request struct {
	Prompt, Model, Provider, Effort, WorkDir, SessionID string
	Attach                                              []string // absolute file paths, task text precedes them
	KeepEnv                                             []string // names the credential scrub must not drop, from --keep-env
	Tools, Autonomous, EnableMemory, Direct             bool     // Direct: agent keeps the provider route, skips the router
	NoTools                                             bool     // claude only: answer from the provider directly, no claude process
	NoPersist                                           bool     // claude, codex: no session persistence
	SafeMode                                            bool     // claude, codex, opencode: no hooks/plugins/customizations
	DebugFile                                           string   // claude, gemini: debug log at <path>
	SystemPrompt, AppendSystemPrompt                    string   // claude only: replace / extend the system prompt
	Env                                                 []string `json:"-"`                   // KEY=VALUE for the child, layered last; never logged
	EnvNames                                            []string `json:"env_names,omitempty"` // the names of Env, all the dispatch log keeps
	OutputSchema                                        string   // JSON Schema text the answer must satisfy
	Persona                                             *Persona `json:"persona,omitempty"` // claude: native agent definition; others: emulated
	Timeout                                             time.Duration
}

// Usage is token accounting where the agent reports it.
type Usage struct {
	InputTokens         int `json:"input_tokens"`                    // uncached input only
	CacheCreationTokens int `json:"cache_creation_tokens,omitempty"` // billed above input; kept apart so cost can be priced
	CacheReadTokens     int `json:"cache_read_tokens,omitempty"`
	OutputTokens        int `json:"output_tokens"`
	// CostUSD is the agent-reported cost where it reports one (claude, agy); absent for the rest.
	CostUSD float64 `json:"cost_usd,omitempty"`
}

// Result is the envelope `ask --format json` prints.
type Result struct {
	Agent           string          `json:"agent"`
	Status          string          `json:"status"` // ok | error | partial | quota | stalled
	Provider        string          `json:"provider,omitempty"`
	Model           string          `json:"model,omitempty"`
	Invoked         string          `json:"invoked_model,omitempty"` // the model (<profile>:<model> when pinned) the run was asked for; not upstream-confirmed
	SessionID       string          `json:"session_id,omitempty"`
	Answer          string          `json:"answer"`
	Usage           Usage           `json:"usage"`
	DurationMs      int64           `json:"duration_ms"`
	Dropped         []string        `json:"dropped,omitempty"`
	SchemaUnchecked []string        `json:"-"`                         // schema keywords the minimal validator does not enforce
	Emulated        []string        `json:"emulated,omitempty"`        // flags the harness shimmed because the agent has no channel
	Fallback        string          `json:"fallback,omitempty"`        // set to the original agent's name when it ran instead
	FallbackReason  string          `json:"fallback_reason,omitempty"` // why the original agent was unavailable
	Handoff         string          `json:"handoff,omitempty"`         // hand-off file written when a copilot leg hit its quota
	Transport       *Transport      `json:"transport,omitempty"`       // stream health; fields an agent can't report stay absent
	Hops            []Hop           `json:"hops,omitempty"`            // every leg RunWithFallback tried, in order
	Structured      json.RawMessage `json:"-"`                         // the answer as JSON satisfying Request.OutputSchema
	recovered       []*Error        // attempts that failed before the successful one, for the hop's errors
}

type coolKey struct{}

// withCoolKey names the provider account a Run under ctx shares its rate limit with.
func withCoolKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, coolKey{}, key)
}

// Hop is one leg RunWithFallback tried; ErrorKind is the envelope error token, empty when the leg answered.
type Hop struct {
	Agent     string `json:"agent"`
	Account   string `json:"account,omitempty"`
	Profile   string `json:"profile,omitempty"`
	Model     string `json:"model,omitempty"`
	ErrorKind string `json:"error_kind,omitempty"`
	// Errors lists every attempt's failure, oldest first, when the leg was retried.
	Errors []EnvelopeError `json:"errors,omitempty"`
}

// Transport is a run's stream health: attempts, retries the agent recovered from, and how steady the stream was.
type Transport struct {
	Attempts      int   `json:"attempts"`                 // tries the outer retry loop made
	Retries       int   `json:"retries,omitempty"`        // api_retry events Claude recovered from
	RetryStatuses []int `json:"retry_statuses,omitempty"` // their HTTP status; 0 = no response
	FirstEventMs  int64 `json:"first_event_ms,omitempty"` // start → first stream_event
	MaxGapMs      int64 `json:"max_gap_ms,omitempty"`     // longest silence between stream_events
}

// Caps declares which Request fields an agent honors; Prompt, Timeout and Attach are always honored
// (every agent has at least an inline-block fallback for Attach).
type Caps struct {
	Model, Provider, Effort, WorkDir, SessionID, Tools, NoTools, Autonomous, Memory, NoPersist, SafeMode, DebugFile, Direct Level
	SystemPrompt, AppendSystemPrompt, Env, OutputSchema, Persona                                                            Level
}

// Level is how an agent honors one flag: its own flag (Native), a harness shim (Emulated), or not at all (None).
type Level uint8

const (
	None Level = iota
	Emulated
	Native
)

func (l Level) String() string { return [...]string{"none", "emulated", "native"}[min(l, Native)] }

// MarshalText renders the level as its name, so describe prints "native" rather than 2.
func (l Level) MarshalText() ([]byte, error) { return []byte(l.String()), nil }

// lv is Native when ok, else None.
func lv(ok bool) Level {
	if ok {
		return Native
	}
	return None
}

// Agent is one agent backend.
type Runner interface {
	Name() string
	Caps() Caps
	Available() error
	Run(ctx context.Context, req Request, sink Sink) (Result, error)
}

// Agents lists every agent in roster order.
func Runners() []Runner {
	return []Runner{claudeRunner{}, vendorRunner{"codex"}, vendorRunner{"gemini"}, vendorRunner{"opencode"}, copilotRunner{}}
}

// Lookup finds an agent by name.
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

// applyCaps clears fields the agent cannot honor and names each one, so a drop is never silent. An Emulated flag
// runs its harness shim here and is named in emulated; a Native one passes through to the adapter's own flag.
func applyCaps(c Caps, req Request) (out Request, dropped, emulated []string) {
	drop := func(l Level, set bool, flag string, clear func()) {
		if set && l == None {
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
	drop(c.NoTools, req.NoTools, "--no-tools", func() { req.NoTools = false })
	drop(c.Autonomous, req.Autonomous, "--autonomous", func() { req.Autonomous = false })
	drop(c.Memory, req.EnableMemory, "--enable-memory", func() { req.EnableMemory = false })
	drop(c.NoPersist, req.NoPersist, "--no-persist", func() { req.NoPersist = false })
	drop(c.SafeMode, req.SafeMode, "--safe-mode", func() { req.SafeMode = false })
	drop(c.Direct, req.Direct, "--direct", func() { req.Direct = false })
	drop(c.DebugFile, req.DebugFile != "", "--debug-file="+req.DebugFile, func() { req.DebugFile = "" })
	drop(c.Env, len(req.Env) > 0, "--env", func() { req.Env = nil })
	drop(c.Persona, req.Persona != nil, "--persona="+personaName(req.Persona), func() { req.Persona = nil })
	if req.Persona != nil && c.Persona == Emulated {
		req, dropped = emulatePersona(c, req, dropped)
		emulated = append(emulated, "--persona")
	}
	if req.SystemPrompt != "" && c.SystemPrompt == Emulated {
		req.Prompt, req.SystemPrompt = preamble(req.SystemPrompt)+req.Prompt, ""
		emulated = append(emulated, "--system-prompt")
	}
	if req.AppendSystemPrompt != "" && c.AppendSystemPrompt == Emulated {
		req.Prompt, req.AppendSystemPrompt = preamble(req.AppendSystemPrompt)+req.Prompt, ""
		emulated = append(emulated, "--append-system-prompt")
	}
	drop(c.SystemPrompt, req.SystemPrompt != "", "--system-prompt", func() { req.SystemPrompt = "" })
	drop(c.AppendSystemPrompt, req.AppendSystemPrompt != "", "--append-system-prompt", func() { req.AppendSystemPrompt = "" })
	if req.OutputSchema != "" && c.OutputSchema != Native { // the answer is validated either way; only the instruction differs
		req.Prompt += schemaInstruction(req.OutputSchema)
		emulated = append(emulated, "--output-schema")
	}
	return req, dropped, emulated
}

// preamble frames system text as instructions ahead of the task, for an agent with no system-prompt channel.
func preamble(text string) string {
	return "System instructions (follow them for this whole task):\n" + text + "\n\nTask:\n"
}

// Run drops unsupported fields loudly, then runs r under DefaultPolicy with req.Timeout per attempt.
func Run(ctx context.Context, r Runner, req Request, sink Sink, notify func(string)) (Result, error) {
	if sink == nil {
		sink = Discard
	}
	req, dropped, emulated := applyCaps(r.Caps(), req)
	res := Result{Agent: r.Name(), Status: StatusError, Dropped: dropped, Emulated: emulated}
	// a no-tools one-shot answers from the provider's API, so it needs no agent binary up front
	if !req.NoTools {
		if err := r.Available(); err != nil {
			return res, &Error{Kind: KindFatal, Agent: r.Name(), Err: err}
		}
	}
	p := DefaultPolicy()
	if req.Timeout > 0 {
		p.AttemptTimeout = req.Timeout
	}
	p.Notify = notify
	p.Key, _ = ctx.Value(coolKey{}).(string)
	if h, _ := ctx.Value(permitKey{}).(*permit); h != nil {
		p.Idle = h.idle
	}
	var recoverErr error
	if _, t, err := resolveTarget(req); err == nil && (t.Provider.Auth.Command != "" || t.Provider.Auth.Sealed()) {
		p.Recover = func(ctx context.Context) error {
			_, recoverErr = providers.Refresh(ctx, t.Provider)
			return recoverErr
		}
	}
	start := time.Now()
	attempts := 0
	var failed []*Error
	e := Do(ctx, p, func(actx context.Context, attempt int) *Error {
		attempts = attempt
		note := func(e *Error) *Error { // every failed attempt, guard failures included, rides on the answering hop
			snap := *e
			snap.Earlier = nil
			failed = append(failed, &snap)
			return e
		}
		// every attempt resumes after a possible idle (cooldown or backoff): another task may have marked the account dead
		if h, _ := ctx.Value(permitKey{}).(*permit); h != nil && h.recheck != nil {
			if skip := h.recheck(); skip != nil {
				skip.terminal = true
				return note(skip)
			}
		}
		actx, gerr := guardAttempt(actx, r, req)
		if gerr != nil {
			return note(gerr)
		}
		cw := &runSink{Sink: sink, agent: r.Name()}
		out, err := r.Run(actx, req, cw)
		// An agent that delegated to another names the actual agent itself;
		// only default to r.Name() when it left Agent unset, so the envelope doesn't lie about who ran.
		out.Agent, out.Dropped, out.Emulated = cmp.Or(out.Agent, res.Agent), append(slices.Clone(dropped), out.Dropped...), emulated
		res = out
		e := Classify(r.Name(), err, cw.n > 0)
		// A hard-quota 429 means this key can never use that model: fall to the next tier instead of waiting it out.
		if e != nil && (e.Kind == KindRateLimit || e.Kind == KindQuota) && providers.ModelDenied(e.Err.Error()) &&
			r.Caps().Model != None && (req.Model == "" || slices.Contains(models.Slots, req.Model)) {
			cur := cmp.Or(req.Model, models.Opus)
			if next := models.NextSlot(cur); next != "" {
				if notify != nil {
					notify(fmt.Sprintf("%s: the %s model is quota-dead on this key; retrying on %s", r.Name(), cur, next))
				}
				req.Model, e.RetryAfter, e.Kind = next, 0, KindRateLimit
			}
		}
		if e != nil {
			note(e)
		}
		return e
	})
	res.DurationMs = time.Since(start).Milliseconds()
	res.Status = statusOf(e)
	if res.Transport == nil {
		res.Transport = &Transport{}
	}
	res.Transport.Attempts = attempts
	if e == nil {
		res.recovered = failed
		if req.OutputSchema != "" { // every agent's answer is checked, native or emulated, so structured behaves alike
			var verr error
			if res.Structured, res.SchemaUnchecked, verr = structuredAnswer(cmp.Or(string(res.Structured), res.Answer), req.OutputSchema); verr != nil {
				e = &Error{Kind: KindFatal, Agent: r.Name(), Err: verr}
				res.Status = statusOf(e)
			}
		}
	}
	if e != nil {
		e.Agent = cmp.Or(e.Agent, r.Name())
		condemnAuth(e, recoverErr)
		return res, e
	}
	return res, nil
}

// Result.Status values.
const (
	StatusOK      = "ok"
	StatusError   = "error"
	StatusPartial = "partial"
	StatusQuota   = "quota"
	StatusStalled = "stalled"
)

// statusOf maps the classified failure to the envelope status: quota, deadline, output-then-failure, else error.
func statusOf(e *Error) string {
	switch {
	case e == nil:
		return StatusOK
	case e.Kind == KindQuota:
		return StatusQuota
	case errors.Is(e, context.DeadlineExceeded):
		return StatusStalled
	case e.Progressed:
		return StatusPartial
	}
	return StatusError
}

// resolveTarget applies --provider then --model (alias, id, or provider:model) to the registry; an id a Claude alias
// currently backs takes the session's alias selection, so CLAUDE_<TIER>_MODEL pins win.
func resolveTarget(req Request) (*providers.Registry, providers.Target, error) {
	reg, err := providers.Load()
	if err != nil {
		return nil, providers.Target{}, err
	}
	// "<profile>:<tier>" resolves like --provider <profile> --model <tier>
	if name, slot, ok := strings.Cut(req.Model, ":"); ok && slices.Contains(models.Slots, slot) && reg.Providers[name] != nil {
		req.Provider, req.Model = name, slot
	}
	sel, err := providers.Select(reg, cmp.Or(req.Provider, registeredSession(reg)))
	if sel.Provider == nil || (err != nil && req.Model == "") {
		return nil, providers.Target{}, err
	}
	if req.Model == "" {
		return reg, sel.Slots[models.Opus], nil
	}
	if t, ok := sel.Slots[req.Model]; ok {
		return reg, t, nil
	}
	if t, ok := aliasTarget(sel, req.Model); ok && !namesProvider(reg, req.Model) {
		return reg, t, nil
	}
	return reg, providers.ParseTarget(reg, req.Model, sel.Provider), nil
}

// aliasTarget is the session's target for a canonical Claude id that an alias backs (any spelling), [1m] kept.
func aliasTarget(sel providers.Selection, model string) (providers.Target, bool) {
	canonical, oneM, ok := models.Resolve(model)
	if !ok {
		return providers.Target{}, false
	}
	t, ok := sel.Slots[models.AliasOf(canonical)]
	if ok && oneM {
		t.OneM = true
	}
	return t, ok
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

// ErrBadAttach marks an unreadable or binary --attach file: a request fault no other agent can fix.
var ErrBadAttach = errors.New("attach")

// attachInline embeds each path as a literal <file path="…">content</file> block after prompt: the
// fallback for an agent with no live-read channel (codex's prompt rides stdin blind).
func attachInline(prompt string, paths []string) (string, error) {
	if len(paths) == 0 {
		return prompt, nil
	}
	var b strings.Builder
	b.WriteString(prompt)
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("%w %s: %w", ErrBadAttach, p, err)
		}
		if IsImage(data) {
			return "", fmt.Errorf("--%w %s: image; this path cannot deliver images (needs file access)", ErrBadAttach, p)
		}
		if !IsText(data) {
			return "", fmt.Errorf("--%w %s: binary file; attach text only", ErrBadAttach, p)
		}
		fmt.Fprintf(&b, "\n\n<file path=%q>\n%s\n</file>", p, data)
	}
	return b.String(), nil
}

// IsImage sniffs data for a png, jpeg, gif or webp header.
func IsImage(data []byte) bool {
	switch http.DetectContentType(data) {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

// splitImages separates the image paths (by content sniff) from the rest; an unreadable path stays in rest so
// attachInline reports it.
func splitImages(paths []string) (images, rest []string) {
	for _, p := range paths {
		head := make([]byte, 512)
		f, err := os.Open(p)
		if err != nil {
			rest = append(rest, p)
			continue
		}
		n, _ := f.Read(head)
		f.Close()
		if IsImage(head[:n]) {
			images = append(images, p)
		} else {
			rest = append(rest, p)
		}
	}
	return images, rest
}

// IsText reports whether data can ride inline in a prompt: valid UTF-8 with no NUL byte.
func IsText(data []byte) bool { return bytes.IndexByte(data, 0) < 0 && utf8.Valid(data) }

// envNames are the names of KEY=VALUE entries.
func envNames(env []string) []string {
	out := make([]string, len(env))
	for i, kv := range env {
		out[i], _, _ = strings.Cut(kv, "=")
	}
	return out
}

// childCmd builds an exec.Cmd in its own process group; a deadline kills the whole tree. The parent env
// is scrubbed of inherited credentials before env (the launch's own additions) is layered on. agent
// also scrubs HARNESS_VAULT_KEY: true for every agent CLI child, false for the claude agent's own child.
func childCmd(ctx context.Context, bin string, argv, env []string, dir, stdin string, keepEnv []string, vendor bool) (*exec.Cmd, *TailBuffer) {
	cmd := exec.CommandContext(ctx, bin, argv...)
	ownGroup(cmd)
	cmd.WaitDelay = 5 * time.Second
	scrub := envscrub.Scrub
	if vendor {
		scrub = envscrub.ScrubVendor
	}
	base, _ := scrub(os.Environ(), envscrub.KeepSet(keepEnv))
	cmd.Env = append(scrubIsolated(ctx, base, keepEnv), env...) // the launch's own values go on after the scrub; later wins
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

// Fallback modes for an unavailable dispatch: the full chain ending in claude, other agents only, or none.
const (
	FallbackAll    = "all"
	FallbackAgents = "agents"
	FallbackNone   = "none"
)

// FallbackModes lists the accepted --fallback values.
var FallbackModes = []string{FallbackAll, FallbackAgents, FallbackNone}

var contentPolicyRE = regexp.MustCompile(`(?i)content[ _-]?(?:policy|filter)|safety (?:system|filter|polic)|` +
	`responsible ai|violates (?:our|the) (?:usage|content) polic|request entity too large|payload too large`)

// requestFault is true when the request itself is at fault (bad attach, HTTP 413, oversized prompt, content-policy
// refusal): another agent would fail the same way, so the loop stops.
func requestFault(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrBadAttach) || errors.Is(err, ErrPromptTooLarge) {
		return true
	}
	if e, ok := errors.AsType[*Error](err); ok && e.Status == 413 {
		return true
	}
	return contentPolicyRE.MatchString(err.Error())
}

func notClaude(name string) bool { return name != "claude" }

// RunWithFallback runs c's legs in turn while the previous one turned out unavailable — binary missing, no provider
// can serve it, a login/quota failure, or an exit with no answer. It never moves on after a timeout, cancellation,
// an oversized prompt or a partial answer, nor when the caller pinned an explicit non-Claude --provider/--model.
// A KindQuota failure marks the leg's account until its reset. The result names the wanted agent in Fallback and
// every gate skip and failed leg in FallbackReason.
func RunWithFallback(ctx context.Context, c Chain, req Request, sink Sink, notify func(string)) (Result, error) {
	if sink == nil {
		sink = Discard
	}
	reasons := slices.Clone(c.Skipped)
	if len(c.Legs) == 0 {
		return Result{Agent: c.Want, Status: StatusError, FallbackReason: strings.Join(reasons, "; ")},
			&Error{Kind: KindFatal, Agent: c.Want, Err: fmt.Errorf("no agent available: %s%s", strings.Join(reasons, "; "), switchHint(reasons))}
	}
	if len(reasons) > 0 && notify != nil {
		notify("skipped without an attempt — " + strings.Join(reasons, "; "))
	}
	var res Result
	var err error
	var prev Runner
	var prevLeg Leg
	stopped := false   // the loop ended on a fail-fast, not by running out of legs
	ran := 0           // index of the last leg that ran
	var handoff string // a copilot quota hand-off survives the legs that run after it
	var hops []Hop
	for i, leg := range c.Legs {
		if i > 0 && !legUnavailable(prev, res, err) { // an answer (or a final failure) ends the chain before any cancel check
			stopped = true
			break
		}
		if cerr := ctx.Err(); cerr != nil { // a cancelled batch must not start another leg
			ce := &Error{Kind: KindFatal, Agent: leg.Agent.Name(), Err: cerr}
			if i == 0 {
				return Result{Agent: ce.Agent, Status: statusOf(ce)}, ce
			}
			err, res.Status, stopped = ce, statusOf(ce), true
			break
		}
		if i > 0 {
			reasons = append(reasons, hopLabel(prevLeg)+": "+fallbackReason(err))
			if notify != nil {
				notify(fmt.Sprintf("%s unavailable (%s) — falling back to %s%s", legLabel(prev), fallbackReason(err), legLabel(leg.Agent), legCell(leg)))
			}
			hop := Event{Type: EventRetry, Agent: leg.Agent.Name(),
				Text: fmt.Sprintf("fallback %s → %s: %s", legLabel(prev), legLabel(leg.Agent), fallbackReason(err))}
			if serr := sink.Emit(hop); serr != nil {
				return res, &Error{Kind: KindFatal, Agent: leg.Agent.Name(), Err: serr}
			}
		}
		lreq := legRequest(req, leg, i == 0 && leg.Agent.Name() == c.Want)
		ctx, leg := freezeCopilot(ctx, leg, lreq)           // per leg: the gate's real target, before any permit or cooldown key
		leg = ownLoginActual(leg, lreq)                     // the hop and its quota key name what the runner resolves
		key := cmp.Or(leg.Account, leg.Agent.Name()+"·own") // own-login legs still share one cap and cooldown per agent
		waitCooldown(ctx, key, DefaultMaxDelay)             // holds nothing
		panicked := false
		func() { // the leg's scope: fail-fast is decided and a panic caught before the permit is released
			release, aerr := acquireLeg(ctx, key) // per billed account, whichever key the task was queued under
			if aerr != nil {
				ce := &Error{Kind: KindFatal, Agent: leg.Agent.Name(), Err: aerr}
				res, err = Result{Agent: ce.Agent, Status: statusOf(ce), Invoked: lreq.Model}, ce
				hops = append(hops, hopOf(leg, lreq, res, err))
				return
			}
			held := &permit{account: key, release: release,
				recheck: func() *Error { return cmp.Or(exhaustedSince(leg), authMarkedSince(leg)) }}
			defer func() {
				if r := recover(); r != nil {
					if ctx.Value(limiterKey{}) == nil {
						held.release()
						panic(r)
					}
					pe := &Error{Kind: KindFatal, Agent: leg.Agent.Name(), Err: fmt.Errorf("panic: %v", r)}
					res, err = Result{Agent: pe.Agent, Status: StatusError, Invoked: lreq.Model}, pe
					hops = append(hops, hopOf(leg, lreq, res, err))
					panicked = true
				}
				if panicked {
					tripFailFast(ctx, Chain{}, 0, req, res, err) // a panic is terminal: no later leg repeats its side effects
				} else {
					tripFailFast(ctx, c, i, req, res, err)
				}
				held.release()
			}()
			// another task may have marked this account dead while this leg waited for its permit
			if skip := cmp.Or(exhaustedSince(leg), authMarkedSince(leg)); skip != nil {
				res, err = Result{Agent: leg.Agent.Name(), Status: statusOf(skip), Invoked: lreq.Model}, skip
				hops = append(hops, hopOf(leg, lreq, res, err))
				return
			}
			res, err = Run(context.WithValue(withCoolKey(ctx, key), permitKey{}, held), leg.Agent, lreq, sink, notify)
			res.Invoked = lreq.Model
			hops = append(hops, hopOf(leg, lreq, res, err))
			noteQuota(leg, err)
			noteAuth(leg, err)
			noteStall(leg, err)
		}()
		if res.Handoff != handoff {
			req.Prompt = promptFromHandoff(res.Handoff, req.Prompt)
		}
		handoff = cmp.Or(res.Handoff, handoff)
		prev, prevLeg, ran = leg.Agent, leg, i
		if panicked { // after the bookkeeping, so a panicking fallback leg is still reported as the fallback
			stopped = true
			break
		}
		if i == 0 && ExplicitBackend(req) {
			stopped = true
			break
		}
	}
	if err != nil && !stopped && len(reasons) > 0 && hopFailed(res, err) {
		err = everyHopFailed(append(reasons, hopLabel(prevLeg)+": "+fallbackReason(err)), err)
	}
	if ran > 0 || c.Legs[ran].otherRoute || c.Legs[ran].Agent.Name() != c.Want { // a different agent or route is a fallback
		res.Fallback, res.FallbackReason = c.Want, strings.Join(reasons, "; ")
	}
	res.Handoff, res.Hops = handoff, hops
	return res, err
}

// promptFromHandoff is the prompt carrying the hand-off doc's progress, else prompt when there is none to read.
func promptFromHandoff(path, prompt string) string {
	if path == "" {
		return prompt
	}
	if doc, err := os.ReadFile(path); err == nil { // later legs pick up the progress, not the bare task
		return handoffPrompt(string(doc))
	}
	return prompt
}

// hopOf records one tried leg: who ran, on which account and profile, and the error token if it failed.
func hopOf(leg Leg, req Request, res Result, err error) Hop {
	h := Hop{Agent: cmp.Or(res.Agent, leg.Agent.Name()), Account: leg.Account, Profile: cmp.Or(res.Provider, req.Provider),
		Model: cmp.Or(res.Model, req.Model)}
	if err != nil {
		h.ErrorKind = errorKind(err)
		if e, ok := errors.AsType[*Error](err); ok && len(e.Earlier) > 0 {
			for _, a := range append(slices.Clone(e.Earlier), e) {
				h.Errors = append(h.Errors, EnvelopeError{Kind: errorKind(a), Message: shapes.Redact(a.attemptMessage())})
			}
		}
	}
	for _, a := range res.recovered { // a leg that answered after retries still lists what it recovered from
		h.Errors = append(h.Errors, EnvelopeError{Kind: errorKind(a), Message: shapes.Redact(a.attemptMessage())})
	}
	return h
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

// hopLabel is legLabel plus the hop's account, "codex (openai·chatgpt)".
func hopLabel(l Leg) string {
	if l.Account == "" {
		return legLabel(l.Agent)
	}
	return legLabel(l.Agent) + " (" + l.Account + ")"
}

// everyHopFailed wraps the last failure with every skip and failed hop, keeping its Kind.
func everyHopFailed(reasons []string, last error) error {
	e, ok := errors.AsType[*Error](last)
	if !ok {
		e = &Error{Kind: KindFatal, Err: last}
	}
	out := *e
	out.Err = fmt.Errorf("every hop failed (%s): %w%s", strings.Join(reasons, "; "), cmp.Or(e.Err, error(e)), switchHint(reasons))
	return &out
}

// switchHint is the D6 hint line when claude was skipped or failed as exhausted, else "".
func switchHint(reasons []string) string {
	for _, r := range reasons {
		if strings.HasPrefix(r, "claude") && strings.Contains(strings.ToLower(r), "exhausted") {
			return "\nrun claude-code switch <profile> to move Claude Code to another profile"
		}
	}
	return ""
}

// legLabel names a leg in fallback reasons: a hub leg by its account, since every hub leg shares the agent name.
func legLabel(r Runner) string {
	if l, ok := r.(hubLeg); ok {
		return l.provider.Name
	}
	return r.Name()
}

// fallbackReason keeps the last line of the failure (an agent CLI prints its own ERROR line last), capped at 200 bytes.
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
	if requestFault(err) {
		return false
	}
	var ae *Error
	if errors.As(err, &ae) && ae.AuthReason != "" && res.Answer == "" {
		return true
	}
	return notClaude(r.Name()) && hopFailed(res, err)
}

// hopFailed is true when a leg failed before giving any answer for a reason a later hop could fix.
func hopFailed(res Result, err error) bool {
	if err == nil || res.Answer != "" || requestFault(err) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
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

// registeredSession is the session's profile when the registry has it, else "": it beats the vault pin.
func registeredSession(reg *providers.Registry) string {
	if name := providers.SessionProfile(); reg != nil && reg.Providers[name] != nil {
		return name
	}
	return ""
}
