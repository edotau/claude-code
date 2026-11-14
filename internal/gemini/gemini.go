package gemini

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentid"

	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

type geminiEngine struct{ agent.BaseEngine }

var (
	_ agent.HeadlessDriver = geminiEngine{}
	_ agent.Authenticator  = geminiEngine{}
)

func (geminiEngine) Attach(prompt string, paths []string, tools bool) (string, error) {
	if tools {
		return agent.AttachSuffix(prompt, paths), nil
	}
	return agent.AttachInline(prompt, paths)
}

func (geminiEngine) Before(req agent.RunOptions, t providers.Target) providers.Target {
	t.Model, _ = agyModelEffort(req, t)
	return t
}

func (geminiEngine) Exec(ctx context.Context, p agent.CLIPrep, req agent.RunOptions, sink wire.Sink) (wire.Result, error) {
	areq := newAgyRequest(p, req)
	res := p.Result
	res.Model = areq.Model
	res.Emulated = append(res.Emulated, p.Emulated...)
	out, err := runAgy(ctx, areq, sink)
	res.Answer, res.Usage, res.Transport, res.Structured = out.Answer, out.Usage, out.Transport, out.Structured
	if out.SessionID != "" {
		res.SessionID = out.SessionID
	}
	return res, err
}

func (geminiEngine) Login(_ *providers.Provider, model string, h *agent.Headless) ([]string, []string, error) {
	return geminiLogin(model, h)
}

// Wire serves only a gateway row: Serves refuses agy every other non-login row.
func (geminiEngine) Wire(ctx context.Context, r agent.WireRoute, h *agent.Headless) ([]string, []string, error) {
	if agent.IsDirect(ctx) { // --direct skips the router that swaps the Databricks token
		return nil, nil, fmt.Errorf("gemini: --direct has no Databricks route for agy (profile %s needs the router's token swap)", r.Provider.Name)
	}
	return geminiGateway(ctx, r.Provider, r.Target.Model, h) // always routed: the router fixes agy's requests for Databricks
}

func (e geminiEngine) FallbackTarget(reg *providers.Registry) (providers.Target, string, bool) {
	present, perr := geminiLoginProbe()
	if perr != nil {
		fmt.Fprintf(os.Stderr, "gemini: agy login marker unreadable, Google-login fallback dropped: %v\n", perr)
	}
	if p, ok := reg.LoginProfile("google", e.Name()); ok && present {
		// the GEMINI_MODEL pin beats the registry's bare slot id
		t, _ := agent.ResolveAgentModel(agent.ModelAsk{Agent: e.Name(), Target: providers.Target{Provider: p, Model: p.Models[models.Opus]}})
		return t, "gemini (Google login via agy)", true
	}
	return providers.Target{}, "", false
}

// AuthStatus is agy's cached Google login from disk; Source is empty and Method is AuthNone when absent, Err when unreadable.
func (geminiEngine) AuthStatus() providers.AuthState {
	ok, err := geminiLoginProbe()
	if !ok {
		return providers.AuthState{Method: providers.AuthNone, Err: err}
	}
	return providers.AuthState{Present: true, Source: "Google login (agy)", Method: providers.AuthCLI}
}

// Plan is empty: agy reports no plan.
func (geminiEngine) Plan(context.Context, providers.AuthState) providers.Subscription {
	return providers.Subscription{}
}

// FollowsPin keeps agy on its Google login unless GEMINI_PROFILE names the session's Databricks row.
func (geminiEngine) FollowsPin(p *providers.Provider) bool {
	if !agent.GeminiGatewayRow(p) {
		return true
	}
	rec, err := agentconfig.Settings(agentid.Gemini)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gemini: agent settings unreadable, pins ignored: %v\n", err)
	}
	return rec.Profile == p.Name
}

// NoRoute refuses a Databricks gateway row that no GEMINI_PROFILE pin selected: never silently.
func (geminiEngine) NoRoute(_ *providers.Registry, t providers.Target) error {
	if agent.GeminiGatewayRow(t.Provider) {
		return errors.New("gemini: agy needs a Google login (agy auth login) or a GEMINI_PROFILE pin")
	}
	return nil
}

func (geminiEngine) Verify(context.Context) (providers.AuthVerdict, string) {
	ok, err := geminiLoginProbe()
	switch {
	case ok:
		return providers.VerdictOK, "agy login present"
	case err != nil:
		return providers.VerdictTransient, "agy login marker unreadable: " + err.Error()
	}
	return providers.VerdictNeedsLogin, ""
}

func (geminiEngine) Render(_ *providers.Registry, sel providers.Selection, home string, o agentconfig.RenderOptions, out io.Writer) (bool, error) {
	reportGemini(sel, out)
	gd, err := renderGeminiSettings(home, o, out)
	if err != nil {
		return gd, err
	}
	hd, err := renderGeminiHooks(home, o, out)
	return gd || hd, err
}

func (geminiEngine) ConfigPaths(home string) ([]agentconfig.LabelPath, error) {
	home, err := agentconfig.HomeOr(home)
	if err != nil {
		return nil, err
	}
	return []agentconfig.LabelPath{
		{Label: "gemini-settings", Path: agySettingsPath(home)},
		{Label: "gemini-hooks", Path: agyHooksPath(home)},
	}, nil
}

func (geminiEngine) RouteDetail() (string, bool) { return geminiRouteDetail(), true }

func (geminiEngine) NewLive() agent.LiveBackend { return &agyLive{} }

// geminiLogin runs agy on its own cached Google sign-in (AgyLoginMarker); without one it errors with the sign-in hint.
func geminiLogin(model string, h *agent.Headless) ([]string, []string, error) {
	present, perr := geminiLoginProbe()
	if perr != nil {
		return nil, nil, fmt.Errorf("gemini: agy login marker unreadable: %w", perr)
	}
	if !present {
		s, _ := agent.LookupSchema("gemini")
		return nil, nil, errors.New("gemini: agy is not signed in; " + s.CLI.ConfigureHint)
	}
	if h != nil {
		return nil, nil, nil // headless argv is runAgy's (prompt over stdin stream-json)
	}
	return nil, []string{"--model", model}, nil
}

// geminiLoginProbe reports whether agy's cached Google login is on disk, plus the first real read failure.
// A missing marker is absent, not an error; agy itself owns and refreshes the token.
func geminiLoginProbe() (bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return false, err
	}
	for _, p := range agyLoginMarkers(home) {
		fi, serr := os.Stat(p)
		if serr == nil && fi.Size() > 0 {
			return true, nil
		}
		if serr != nil && !errors.Is(serr, fs.ErrNotExist) && err == nil {
			err = serr
		}
	}
	return false, err
}

// AgyLoginMarker is agy's cached Google OAuth token path under home, the marker a login run leaves behind.
// agy 1.4.2 moved this from jetski-standalone-oauth-token to antigravity-cli/antigravity-oauth-token;
// this is the current path a fresh sign-in writes.
func AgyLoginMarker(home string) string {
	return agyLoginMarkers(home)[0]
}

// agyLoginMarkers lists every path agy has used for its cached Google OAuth token, newest first.
func agyLoginMarkers(home string) []string {
	return []string{
		filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token"),
		filepath.Join(home, ".gemini", "jetski-standalone-oauth-token"),
	}
}

// agyDefaultModel runs agy when the caller names no model: fast, and enough for headless dispatches.
const agyDefaultModel = "gemini-3.8-flash-high"

// agyGatewayArg is model's Databricks name and gateway --model; a non-Gemini id falls back to the pin, then the default.
func agyGatewayArg(model string) (base, arg string, err error) {
	base, effort, ok := agyGatewayBase(model)
	if !ok {
		if geminiNamed(model) {
			return "", "", fmt.Errorf("gemini: %q is not a catalogued Gemini model; pass a catalogued id with --model", model)
		}
		rec, err := agentconfig.Settings(agentid.Gemini)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gemini: agent settings unreadable, pins ignored: %v\n", err)
		}
		if base, effort, ok = agyGatewayBase(rec.Model); !ok {
			if geminiNamed(rec.Model) {
				return "", "", fmt.Errorf("gemini: the GEMINI_MODEL pin %q is not a catalogued Gemini model; fix or clear the pin", rec.Model)
			}
			base, effort, _ = agyGatewayBase(agyDefaultModel) // a non-Gemini pin must not leave base empty
		}
	}
	if effort == "" {
		return base, base, nil
	}
	return base, base + "-" + effort, nil
}

// geminiNamed reports an id that claims to be a Gemini model (a Databricks Claude slot never does).
func geminiNamed(id string) bool { return strings.Contains(strings.ToLower(id), "gemini") }

// agyGatewayBase resolves id (bare, dotted, Databricks-spelled, or effort-suffixed) to the Databricks endpoint name.
func agyGatewayBase(id string) (base, effort string, ok bool) {
	if before, suffix, ok := splitEffort(id); ok {
		if b, _, ok := agyGatewayBase(before); ok {
			return b, suffix, true
		}
	}
	canonical, _, ok := models.Resolve(id)
	if !ok || models.FamilyKind(canonical) != models.FamilyGemini {
		return "", "", false
	}
	return models.Wire("databricks-serving", canonical), "", true // agy lists it as written: the name shows Databricks
}

// geminiGateway runs agy on the router's /p/<profile>/gemini route; agy holds only a profile-scoped session token.
func geminiGateway(ctx context.Context, p *providers.Provider, model string, h *agent.Headless) ([]string, []string, error) {
	base, arg, err := agyGatewayArg(model)
	if err != nil {
		return nil, nil, err
	}
	rb, err := agent.EnsureRouter(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("gemini: router: %w", err)
	}
	tok, err := agent.SessionToken(ctx, rb, p.Name)
	if err != nil {
		return nil, nil, fmt.Errorf("gemini: session token for profile %s: %w", p.Name, err)
	}
	list, lite := agyGatewayModels(p, base, time.Now())
	env := proc.Values{
		"AGY_LLM_GATEWAY_URL":     gateway.SessionBase(rb, p.Name) + "/" + providers.ProtocolGemini,
		"AGY_LLM_GATEWAY_API_KEY": tok, "AGY_LLM_GATEWAY_WIRE_PROTOCOL": "genai", "AGY_LLM_GATEWAY_MODELS": list,
		// agy's side calls (titles) use the flash-lite tier, kept off the main model's rate limit when served
		"AGY_LLM_GATEWAY_TIER_FLASH": base, "AGY_LLM_GATEWAY_TIER_PRO": base, "AGY_LLM_GATEWAY_TIER_FLASH_LITE": lite,
		// a caller's own gateway headers, CA bundle or proxy must not reach the loopback router
		"AGY_LLM_GATEWAY_HEADERS": "", "AGY_LLM_GATEWAY_CA_CERT": "", "AGY_LLM_GATEWAY_PROXY_URL": "",
	}
	if h != nil {
		return env.Env(), nil, nil // headless argv is runAgy's (prompt over stdin stream-json)
	}
	argv := []string{"--model", base} // the gateway advertises only the base id
	if effort := agyArgEffort(base, arg); effort != "" {
		argv = append(argv, "--effort", effort)
	}
	return env.Env(), argv, nil
}

// agyFlashLite is the model agy's flash-lite tier runs when the profile serves it.
const agyFlashLite = "databricks-gemini-3-1-flash-lite"

// agyServedGemini lists the READY Gemini endpoint names in p's fresh cached listing; nil when cold. A var for tests.
var agyServedGemini = func(p *providers.Provider, now time.Time) []string {
	s, ok := router.LoadServed(p.Name)
	if !ok || !s.Fresh(now) {
		return nil
	}
	var out []string
	for _, m := range s.Models {
		base, _, ok := agyGatewayBase(m.ID)
		if ok && (m.Ready == "" || m.Ready == "READY") && !slices.Contains(out, base) {
			out = append(out, base)
		}
	}
	return out
}

// agyGatewayModels is agy's advertised list (base first, then every other served Gemini model) and flash-lite tier.
func agyGatewayModels(p *providers.Provider, base string, now time.Time) (list, lite string) {
	served := agyServedGemini(p, now)
	others := slices.DeleteFunc(slices.Clone(served), func(m string) bool { return m == base })
	lite = base
	if slices.Contains(served, agyFlashLite) {
		lite = agyFlashLite
	}
	return strings.Join(append([]string{base}, others...), ","), lite
}

// agyRequest is one headless agy run: the prompt rides stdin as a single stream-json NDJSON line.
// Env carries extra launch vars; agy authenticates with its own cached Google sign-in, never a key.
type agyRequest struct {
	Prompt, Model, Cwd, SessionID string
	Effort                        string // a gateway pin's effort suffix; a --effort in SettingsArgv wins
	Tools, Autonomous             bool
	DebugFile                     string   // --log-file
	OutputSchema                  string   // --json-schema
	SettingsArgv                  []string // RenderSettings' --effort and --settings fragment, inserted before -p=
	Permissions                   agent.Permissions
	Env, KeepEnv                  []string
}

// effortSuffixed reports a model id that already names its effort (gemini-3.8-flash-high).
func effortSuffixed(model string) bool {
	_, _, ok := splitEffort(model)
	return ok
}

// splitEffort cuts a trailing native effort off id.
func splitEffort(id string) (base, effort string, ok bool) {
	base, effort, cut := strings.CutLast(id, "-")
	return base, effort, cut && slices.Contains(agentid.NativeEfforts(agentid.Gemini), effort)
}

// agyToolLessPreamble is agy's own verified tool-less instruction (no tools-off flag exists).
const agyToolLessPreamble = "Answer from the text below only. Do not call any tools.\n\n"

// agyArgv builds agy's headless argv: stream-json both ways, the prompt rides stdin behind -p=.
func agyArgv(r agyRequest) []string {
	argv := []string{"--model", r.Model}
	// agy refuses --effort beside an effort-suffixed id; a --effort in SettingsArgv (flag or record) beats the pin suffix
	if slices.Contains(agentid.NativeEfforts(agentid.Gemini), r.Effort) && !effortSuffixed(r.Model) && !slices.Contains(r.SettingsArgv, "--effort") {
		argv = append(argv, "--effort", r.Effort)
	}
	if r.SessionID != "" { // resume a prior agy conversation rather than starting fresh
		argv = append(argv, "--conversation", r.SessionID)
	}
	argv = append(argv, "--input-format", "stream-json", "--output-format", "stream-json")
	if r.OutputSchema != "" {
		argv = append(argv, "--json-schema", r.OutputSchema)
	}
	if r.Tools {
		argv = append(argv, "--dangerously-skip-permissions")
	}
	if r.Autonomous {
		argv = append(argv, "--mode", "auto")
	}
	if r.DebugFile != "" {
		argv = append(argv, "--log-file", r.DebugFile)
	}
	return append(append(argv, r.SettingsArgv...), "-p=")
}

// agyStdin is the one NDJSON line agy reads behind --input-format stream-json.
func agyStdin(r agyRequest) (string, error) {
	content := r.Prompt
	if !r.Tools {
		content = agyToolLessPreamble + content
	}
	line, err := json.Marshal(map[string]any{"event": "user", "message": map[string]any{"content": content}})
	if err != nil {
		return "", fmt.Errorf("agy: encode stdin: %w", err)
	}
	return string(line) + "\n", nil
}

// runAgy drives `agy` headless over stream-json: agent_response text deltas stream live, and the
// result event carries the final answer, usage, conversation id and status.
func runAgy(ctx context.Context, r agyRequest, sink wire.Sink) (wire.Result, error) {
	bin, err := paths.AgentBin("agy")
	if err != nil {
		return wire.Result{}, fmt.Errorf("gemini: %w", err)
	}
	stdin, err := agyStdin(r)
	if err != nil {
		return wire.Result{}, err
	}
	cmd, tail := agent.ChildCmd(ctx, bin, agyArgv(r), r.Env, r.Cwd, stdin, r.KeepEnv, true)
	dn := &agyDenier{p: r.Permissions, cwd: r.Cwd}
	parse := func(br *bufio.Reader, sink wire.Sink, _ func(), logs ...wire.StreamLog) (wire.Result, error) {
		return parseAgyEvents(br, sink, false, wire.PickLog(logs), dn)
	}
	return agent.RunEventStream(ctx, "gemini", cmd, tail, sink, parse, agent.StreamOpts{})
}

// agyToolStep is the step_type of a tool execution, seen live as `tool` with states active then DONE.
const agyToolStep = "tool"

// agyEvent mirrors agy's stream-json schema: one JSON object per line.
type agyEvent struct {
	Event      string `json:"event"`
	ConvID     string `json:"conversation_id"` // init event only
	StepUpdate struct {
		StepType  string `json:"step_type"`
		State     string `json:"state"`
		TextDelta string `json:"text_delta"`
		ToolName  string `json:"tool_name"`
		ToolInfo  struct {
			Parameters json.RawMessage `json:"parameters"`
			Error      struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"tool_info"`
	} `json:"step_update"`
	Result struct {
		ConversationID string          `json:"conversation_id"`
		Status         string          `json:"status"`
		Response       string          `json:"response"`
		Structured     json.RawMessage `json:"structured_output"`
		Error          string          `json:"error"`
		Usage          struct {
			InputTokens     int     `json:"input_tokens"`
			OutputTokens    int     `json:"output_tokens"`
			ThinkingTokens  int     `json:"thinking_tokens"`
			CacheReadTokens int     `json:"cache_read_tokens"`
			CostUSD         float64 `json:"cost_usd"` // not reported by agy today; read if it ever is
		} `json:"usage"`
		DeniedActions []struct {
			Action      string `json:"action"`
			DisplayName string `json:"display_name"`
		} `json:"denied_actions"`
	} `json:"result"`
}

// parseAgyStream reads `agy --output-format stream-json`: agent_response deltas, then one result event.
func parseAgyStream(br *bufio.Reader, sink wire.Sink, onMeaningful func(), logs ...wire.StreamLog) (wire.Result, error) {
	return parseAgyEvents(br, sink, false, wire.PickLog(logs), nil)
}

// parseAgyEvents stops at the first result when once is set, preserving buffered lines for the next turn.
func parseAgyEvents(br *bufio.Reader, sink wire.Sink, once bool, lg wire.StreamLog, dn *agyDenier) (res wire.Result, _ error) {
	t := &agyTurn{sink: sink, lg: lg, once: once, res: &res, dn: dn}
	onBad := func(err error) { t.flush(); _ = wire.EmitError(lg, sink, err.Error()) } // the decode error aborts
	tr, err := wire.DecodeEvents(br, lg, "decode agy stream-json", true, onBad, func(ev agyEvent, _ *wire.TransportTracker) (bool, error) {
		return t.handle(ev)
	})
	res.Transport = tr
	if err != nil {
		if t.pending.Len() > 0 { // a decode error already flushed; a sink error did not
			t.flush()
		}
		res.Answer = cmp.Or(t.ans.String(), strings.TrimSpace(t.response))
		return res, err
	}
	if !t.sawResult {
		t.flush()
		res.Answer = t.ans.String()
		return res, errors.New("agy: stream ended without a result event")
	}
	return finishAgyTurn(res, sink, &t.ans, t.response, t.flush, t.openStep, once)
}

// agyTurn is parseAgyEvents' state across one turn's stream-json events.
type agyTurn struct {
	sink      wire.Sink
	lg        wire.StreamLog
	once      bool
	res       *wire.Result
	dn        *agyDenier // nil: refused calls are not explained
	refused   []agent.Rule
	ans       wire.Answer
	pending   strings.Builder
	response  string
	sawResult bool
	lastStep  string // the last step type/state logged, so repeated updates stay one line
	openStep  string // the last non-response step still running (a backgrounded command), else ""
	toolOpen  bool   // a tool step is running, so one tool_use is emitted per step, not per update
}

func (t *agyTurn) flush() {
	t.lg.Emit("assistant", t.pending.String())
	t.pending.Reset()
}

// handle applies one event; done reports that a once-turn reached its result.
func (t *agyTurn) handle(ev agyEvent) (done bool, _ error) {
	switch ev.Event {
	case "init":
		t.res.SessionID = ev.ConvID
	case "step_update":
		return false, t.step(ev)
	case "result":
		return t.result(ev)
	}
	return false, nil
}

func (t *agyTurn) step(ev agyEvent) error {
	noteAgyStep(ev, &t.lastStep, &t.openStep, t.lg)
	if call, ok := agyRefusedCall(ev); ok {
		t.refused = append(t.refused, call)
	}
	if err := emitAgyToolStart(ev, &t.toolOpen, t.sink); err != nil {
		return err
	}
	if ev.StepUpdate.StepType != "agent_response" || ev.StepUpdate.TextDelta == "" {
		return nil
	}
	t.pending.WriteString(ev.StepUpdate.TextDelta)
	return t.ans.Emit(t.sink, ev.StepUpdate.TextDelta, false)
}

func (t *agyTurn) result(ev agyEvent) (bool, error) {
	t.sawResult = true
	t.response = ev.Result.Response
	t.flush()
	err := applyAgyResult(t.res, ev)
	t.dn.report(t.refused)
	if err != nil {
		_ = wire.EmitError(t.lg, t.sink, err.Error()) // the agy error returned below aborts
		if t.once {
			err = wire.NewResultError(err) // the result was consumed: the turn failed, the stream is at a boundary
		}
		return false, err
	}
	t.lg.Emit("result", cmp.Or(t.response, t.ans.String()))
	return t.once, nil
}

// emitAgyToolStart marks progress once per tool step: agy runs tools without tool events, so none can be shown.
func emitAgyToolStart(ev agyEvent, toolOpen *bool, sink wire.Sink) error {
	if ev.StepUpdate.StepType != agyToolStep {
		return nil
	}
	open := !agyStepEnded(ev.StepUpdate.State)
	start := open && !*toolOpen
	*toolOpen = open
	if !start {
		return nil
	}
	return sink.Emit(wire.Event{Type: wire.EventProgress})
}

// noteAgyStep tracks the unfinished non-response step and logs each new step type/state once.
func noteAgyStep(ev agyEvent, lastStep, openStep *string, lg wire.StreamLog) {
	if t, st := ev.StepUpdate.StepType, ev.StepUpdate.State; t != "agent_response" && t != "user_input" && st != "" {
		if !agyStepEnded(st) {
			*openStep = t
		} else if *openStep == t {
			*openStep = ""
		}
		if key := t + "/" + st; key != *lastStep {
			*lastStep = key
			lg.Emit("step", t+" "+strings.ToLower(st))
		}
	}
}

// agyStepEnded: DONE and ERROR both end a step; a tool call a hook refused ends in ERROR, not unfinished.
func agyStepEnded(state string) bool { return state == "DONE" || state == "ERROR" }

// finishAgyTurn flushes the pending log line and sets the answer; a run whose text arrived only in
// result.response (no agent_response deltas) still answers.
func finishAgyTurn(res wire.Result, sink wire.Sink, ans *wire.Answer, response string, flush func(), openStep string, once bool) (wire.Result, error) {
	flush()
	var emitErr error
	if ans.Empty() && response != "" {
		emitErr = ans.Emit(sink, response, false)
	}
	res.Answer = ans.String()
	if emitErr != nil {
		return res, emitErr
	}
	if !once && ((res.Answer == "" && len(res.Structured) == 0) || openStep != "") {
		why := "empty answer"
		if openStep != "" {
			why = "turn ended with an unfinished " + openStep + " step"
		}
		return res, &retry.Error{Kind: retry.KindFatal, Agent: "gemini", Progressed: true, Err: errors.New("agy: partial result: " + why)}
	}
	return res, nil
}

func applyAgyResult(res *wire.Result, ev agyEvent) error {
	out := ev.Result
	if out.ConversationID != "" {
		res.SessionID = out.ConversationID
	}
	res.Usage = wire.Usage{
		InputTokens:     out.Usage.InputTokens,
		OutputTokens:    out.Usage.OutputTokens + out.Usage.ThinkingTokens,
		CacheReadTokens: out.Usage.CacheReadTokens,
		CostUSD:         out.Usage.CostUSD,
	}
	if len(out.Structured) > 0 && string(out.Structured) != "null" {
		res.Structured = out.Structured
	}
	if len(out.DeniedActions) > 0 {
		names := make([]string, len(out.DeniedActions))
		for i, d := range out.DeniedActions {
			names[i] = cmp.Or(d.DisplayName, d.Action)
		}
		fmt.Fprintf(os.Stderr, "agy: denied %s\n", strings.Join(names, ", "))
	}
	if out.Status != "SUCCESS" {
		return errors.New(cmp.Or(out.Error, "agy run "+cmp.Or(out.Status, "failed")))
	}
	return nil
}

// --- gemini: no auth-type setting to render (auth is the cached login marker, see geminiLoginProbe); guard denies
// and hooks are rendered separately by renderGeminiSettings and renderGeminiHooks.

func reportGemini(sel providers.Selection, out io.Writer) {
	if sel.Provider.Auth.Method() == providers.AuthCLI {
		fmt.Fprintln(out, "gemini: skipped: session provider is passthrough, not keyed")
		return
	}
	fmt.Fprintln(out, "gemini: no auth setting to render; agy has no settings-auth equivalent (launch-time env/login only)")
}

// geminiRouteDetail reports agy's own GEMINI_MODEL pin (effort rides the id's own suffix) — unset
// means agy's own default (agyDefaultModel).
func geminiRouteDetail() string {
	model, source := providers.GeminiModel()
	return fmt.Sprintf("model=%s source=%s", cmp.Or(model, "(default)"), source)
}

// agyModelEffort is the --model agy runs for req on t and a gateway row's pinned effort ("" elsewhere).
func agyModelEffort(req agent.RunOptions, t providers.Target) (model, effort string) {
	if req.Model == "" || t.Model == "" { // the slot table's opus id isn't agy's own: GEMINI_MODEL pin, else flash-high
		t.Model = agyDefaultModel
	}
	gt, _ := agent.ResolveAgentModel(agent.ModelAsk{Agent: "gemini", Flag: req.Model, Target: t, Applied: true})
	if !agent.GeminiGatewayRow(t.Provider) {
		return gt.Model, ""
	}
	base, arg, err := agyGatewayArg(gt.Model) // gateway agy lists only the base id; the effort rides --effort
	if err != nil {
		return gt.Model, "" // geminiGateway refuses it before the launch
	}
	return base, agyArgEffort(base, arg)
}

// agyArgEffort is the effort suffix agyGatewayArg appended to base in arg ("" when none).
func agyArgEffort(base, arg string) string {
	return strings.TrimPrefix(strings.TrimPrefix(arg, base), "-")
}

// newAgyRequest is the agy run for req: only the resolved credential env carries over; agyArgv builds the argv.
func newAgyRequest(p agent.CLIPrep, req agent.RunOptions) agyRequest {
	model, effort := agyModelEffort(req, p.Target)
	return agyRequest{
		Prompt: p.Prompt, Model: model, Effort: effort, Cwd: req.WorkDir, Tools: req.Tools, Autonomous: req.Autonomous,
		DebugFile: req.DebugFile, Env: p.Env, KeepEnv: req.KeepEnv, SessionID: req.SessionID, OutputSchema: req.OutputSchema,
		SettingsArgv: p.Argv, Permissions: req.Permissions,
	}
}

// agyLive holds `agy --input-format stream-json`: one conversation, one NDJSON line per turn.
type agyLive struct {
	tools   bool
	dn      *agyDenier
	intro   string // emulated system prompts, owed to the first turn only
	resumed bool   // a --session conversation: agy's cumulative usage already holds its history
	prev    wire.Usage
}

func (b *agyLive) Build(ctx context.Context, req agent.RunOptions) (*exec.Cmd, *retry.TailBuffer, string, error) {
	ctx = agent.DirectCtx(ctx, req)
	req.Prompt = cmp.Or(req.Prompt, "live session") // prepare wants a prompt; the turns carry the real ones
	p, err := agent.PrepareCLI(ctx, "gemini", req)
	if err != nil {
		return nil, nil, "", err
	}
	areq := newAgyRequest(p, req)
	bin, err := paths.AgentBin("agy")
	if err != nil {
		return nil, nil, "", fmt.Errorf("gemini: %w", err)
	}
	b.tools, b.resumed, b.dn = areq.Tools, req.SessionID != "", &agyDenier{p: areq.Permissions, cwd: areq.Cwd}
	if req.SystemPrompt != "" { // agy has no system-prompt flag: the one-shot path prepends it, so does the first turn
		b.intro += agent.Preamble(req.SystemPrompt)
	}
	if req.AppendSystemPrompt != "" {
		b.intro += agent.Preamble(req.AppendSystemPrompt)
	}
	cmd, tail := agent.ChildCmd(ctx, bin, agyArgv(areq), areq.Env, areq.Cwd, "", areq.KeepEnv, true)
	return cmd, tail, p.Result.Provider, nil
}

func (b *agyLive) WriteTurn(w io.Writer, prompt string) error {
	prompt, b.intro = b.intro+prompt, ""
	line, err := agyStdin(agyRequest{Prompt: prompt, Tools: b.tools})
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, line)
	return err
}

// Parse subtracts the usage already reported: agy's result usage is cumulative over the conversation.
func (b *agyLive) Parse(dec *bufio.Reader, sink wire.Sink, lg func(kind, text string)) (wire.Result, error) {
	res, err := parseAgyEvents(dec, sink, true, lg, b.dn)
	if err != nil && !agent.IsResultError(err) {
		return res, err
	}
	total := res.Usage
	if b.resumed { // the history behind a --session is unknown: this turn reports no usage, later ones their delta
		b.resumed, b.prev, res.Usage = false, total, wire.Usage{}
		return res, err
	}
	res.Usage = wire.Usage{InputTokens: max(total.InputTokens-b.prev.InputTokens, 0), OutputTokens: max(total.OutputTokens-b.prev.OutputTokens, 0),
		CacheReadTokens: max(total.CacheReadTokens-b.prev.CacheReadTokens, 0), CostUSD: max(total.CostUSD-b.prev.CostUSD, 0)}
	b.prev = total
	return res, err
}

func init() {
	agent.Register(agent.Agent{Row: row, Driver: geminiEngine{agent.NewBaseEngine("gemini")}})
}

// row is gemini's roster entry: binary, dialects, capabilities, events and launch hints.
var row = agent.Schema{Name: "gemini", CLI: agent.CLI{
	PromptVia: agent.PromptStdin,
	// agy print mode drops a single message's text past ~100–200 KB (probe 2026-11-05: found at 100 KB, lost at 200 KB)
	PromptMaxBytes: 100 * 1024,
	// agy print mode cannot carry a large message but reads a 300 KB file with tools (probe 2026-11-05, ZEBRA-290)
	PackVia: agent.PackFile,
	Binary:  "agy",
	Caps: agent.Caps{Model: agent.Native, Provider: agent.Native, Effort: agent.Native, WorkDir: agent.Native, SessionID: agent.Native,
		Tools: agent.Native, ReadOnly: agent.Emulated, Autonomous: agent.Native, DebugFile: agent.Native, Direct: agent.Native, Env: agent.Native,
		SystemPrompt: agent.Emulated, AppendSystemPrompt: agent.Emulated, OutputSchema: agent.Native, Persona: agent.Emulated,
		Permissions: agent.Native},
	Events: []string{wire.EventStart, wire.EventText, wire.EventProgress, wire.EventResult, wire.EventError}, Cost: true,
	ResumeFormat: "agy --conversation %s", ModelsArgv: []string{"models"}, ReadNeedsTools: true,
	ConfigureHint: "sign in with `agy` (Google login)", DisplayName: "antigravity", LoginArgv: []string{"agy"},
	Extras: map[string][]string{extraUseGuardRules: {"true", "false"}},
}}

// extraUseGuardRules gates the guard denies in agy's settings and --settings; only "false" turns them off.
const extraUseGuardRules = "useGuardRules"
