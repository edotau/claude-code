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
	"sync"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentconfig/render"
	"github.com/edotau/claude-code/internal/vault"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
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
	t.Model = agyModel(req, t)
	return t
}

func (geminiEngine) Exec(ctx context.Context, p agent.CLIPrep, req agent.RunOptions, sink agent.Sink) (agent.Result, error) {
	areq := newAgyRequest(p, req)
	res := p.Result
	res.Model = areq.Model
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
	if p, ok := agent.LoginProvider(reg, "google", e.Name()); ok && GeminiLoginPresent() {
		// the GEMINI_MODEL pin beats the registry's bare slot id
		t, _ := agent.ResolveAgentModel(agent.ModelAsk{Agent: e.Name(), Target: providers.Target{Provider: p, Model: p.Models[models.Opus]}})
		return t, "gemini (Google login via agy)", true
	}
	return providers.Target{}, "", false
}

// AuthStatus is agy's cached Google login from disk; Source and Method are empty when absent, Err when unreadable.
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
	return !agent.GeminiGatewayRow(p) || geminiRecord().Profile == p.Name
}

// NoRoute refuses a Databricks gateway row that no GEMINI_PROFILE pin selected: never silently.
func (geminiEngine) NoRoute(_ *providers.Registry, t providers.Target) error {
	if agent.GeminiGatewayRow(t.Provider) {
		return errors.New("gemini: agy needs a Google login (agy auth login) or a GEMINI_PROFILE pin")
	}
	return nil
}

// geminiRecord is the gemini pin record; an unreadable vault warns on stderr and reads as unset.
func geminiRecord() vault.AgentRecord {
	recs, err := vault.ReadAgentRecords([]string{"gemini"})
	if err != nil {
		fmt.Fprintf(os.Stderr, "gemini: vault unreadable, pins ignored: %v\n", err)
	}
	return recs["gemini"]
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

func (geminiEngine) Render(_ *providers.Registry, sel providers.Selection, home string, o render.RenderOptions, out io.Writer) (bool, error) {
	d, err := reportGemini(sel, out)
	if err != nil {
		return d, err
	}
	gd, err := renderGeminiSettings(home, o, out)
	return d || gd, err
}

func (geminiEngine) ConfigPaths(home string) ([]render.LabelPath, error) {
	home, err := render.HomeOr(home)
	if err != nil {
		return nil, err
	}
	return []render.LabelPath{{Label: "gemini-settings", Path: filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")}}, nil
}

func (geminiEngine) RouteDetail() (string, bool) { return geminiRouteDetail(), true }

func (geminiEngine) NewLive() agent.LiveBackend { return &agyLive{} }

// geminiLogin runs agy on its own cached Google sign-in (AgyLoginMarker); without one it errors with the sign-in hint.
func geminiLogin(model string, h *agent.Headless) ([]string, []string, error) {
	if !GeminiLoginPresent() {
		s, _ := agent.LookupSchema("gemini")
		return nil, nil, errors.New("gemini: agy is not signed in; " + s.CLI.ConfigureHint)
	}
	if h != nil {
		return nil, nil, nil // headless argv is agent.RunAgy's (prompt over stdin stream-json)
	}
	return nil, []string{"--model", model}, nil
}

// GeminiLoginPresent reports whether agy's cached Google login is on disk; agy itself owns and refreshes the token.
func GeminiLoginPresent() bool {
	ok, _ := geminiLoginProbe()
	return ok
}

// geminiLoginProbe is GeminiLoginPresent plus the first real read failure; a missing marker is absent, not an error.
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

// AgyDefaultModel runs agy when the caller names no model: fast, and enough for headless dispatches.
const AgyDefaultModel = "gemini-3.8-flash-high"

// agyEfforts are the effort suffixes agy bakes into its model ids (gemini-3.8-flash-high).
var agyEfforts = []string{"low", "medium", "high", "xhigh", "max"}

// agyGatewayArg returns the dotted catalog id agy lists for model (effort suffix dropped) and the --model agy takes in
// gateway mode (base plus model's effort suffix); a non-Gemini id (a Databricks Claude slot) falls back to the
// GEMINI_MODEL pin, else AgyDefaultModel.
func agyGatewayArg(model string) (base, arg string) {
	base, effort, ok := agyGatewayBase(model)
	if !ok {
		base, effort, _ = agyGatewayBase(cmp.Or(geminiRecord().Model, AgyDefaultModel))
	}
	if effort == "" {
		return base, base
	}
	return base, base + "-" + effort
}

// agyGatewayBase resolves id (bare, dotted, Databricks-spelled, or effort-suffixed) to a Gemini catalog id.
func agyGatewayBase(id string) (base, effort string, ok bool) {
	if before, suffix, cut := strings.CutLast(id, "-"); cut && slices.Contains(agyEfforts, suffix) {
		if b, _, ok := agyGatewayBase(before); ok {
			return b, suffix, true
		}
	}
	canonical, _, ok := models.Resolve(id)
	if !ok || models.FamilyKind(canonical) != models.FamilyGemini {
		return "", "", false
	}
	return models.Wire("dotted", canonical), "", true
}

// gatewayEntry is one router+profile token slot; its own lock keeps a slow mint from stalling other profiles.
type gatewayEntry struct {
	mu  sync.Mutex
	tok string
}

// gatewayTokens holds one router session token per router base and profile; a new router gets a fresh mint.
var (
	gatewayMu     sync.Mutex
	gatewayTokens = map[string]*gatewayEntry{}
)

// gatewayToken mints profile's token once per router (concurrent --batch jobs share it), under the router's token cap.
func gatewayToken(ctx context.Context, routerBase, profile string) (string, error) {
	gatewayMu.Lock()
	e := gatewayTokens[routerBase+"|"+profile]
	if e == nil {
		e = &gatewayEntry{}
		gatewayTokens[routerBase+"|"+profile] = e
	}
	gatewayMu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.tok != "" {
		return e.tok, nil
	}
	tok, err := agent.MintSession(ctx, profile, os.Getpid())
	if err != nil {
		return "", err
	}
	e.tok = tok
	return tok, nil
}

// geminiGateway runs agy in its env-only gateway mode against the router's /p/<profile>/gemini route, which swaps
// in a fresh Databricks token per request; the key agy holds is a session token scoped to this profile.
func geminiGateway(ctx context.Context, p *providers.Provider, model string, h *agent.Headless) ([]string, []string, error) {
	rb, err := agent.EnsureRouter(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("gemini: router: %w", err)
	}
	tok, err := gatewayToken(ctx, rb, p.Name)
	if err != nil {
		return nil, nil, fmt.Errorf("gemini: session token for profile %s: %w", p.Name, err)
	}
	base, arg := agyGatewayArg(model)
	env := []string{
		"AGY_LLM_GATEWAY_URL=" + proxy.SessionBase(rb, p.Name) + "/" + providers.ProtocolGemini,
		"AGY_LLM_GATEWAY_API_KEY=" + tok,
		"AGY_LLM_GATEWAY_WIRE_PROTOCOL=genai",
		"AGY_LLM_GATEWAY_MODELS=" + base,
		// every tier on the one served model: agy's side calls (titles) use the flash-lite tier
		"AGY_LLM_GATEWAY_TIER_FLASH=" + base, "AGY_LLM_GATEWAY_TIER_PRO=" + base, "AGY_LLM_GATEWAY_TIER_FLASH_LITE=" + base,
		// a caller's own gateway headers, CA bundle or proxy must not reach the loopback router
		"AGY_LLM_GATEWAY_HEADERS=", "AGY_LLM_GATEWAY_CA_CERT=", "AGY_LLM_GATEWAY_PROXY_URL=",
	}
	if h != nil {
		return env, nil, nil // headless argv is agent.RunAgy's (prompt over stdin stream-json)
	}
	return env, []string{"--model", arg}, nil
}

// agyRequest is one headless agy run: the prompt rides stdin as a single stream-json NDJSON line.
// Env carries extra launch vars; agy authenticates with its own cached Google sign-in, never a key.
type agyRequest struct {
	Prompt, Model, Effort, Cwd, SessionID string
	Tools, Autonomous                     bool
	DebugFile                             string // --log-file
	OutputSchema                          string // --json-schema
	Env, KeepEnv                          []string
}

// agyEffort maps the harness's own effort vocabulary onto agy's (identity today; unknown values drop).
var agyEffort = map[string]string{"low": "low", "medium": "medium", "high": "high", "xhigh": "xhigh", "max": "max"}

// agyToolLessPreamble is agy's own verified tool-less instruction (no tools-off flag exists).
const agyToolLessPreamble = "Answer from the text below only. Do not call any tools.\n\n"

// agyArgv builds agy's headless argv: stream-json both ways, the prompt rides stdin behind -p=.
func agyArgv(r agyRequest) []string {
	argv := []string{"--model", r.Model}
	if e := agyEffort[r.Effort]; e != "" {
		argv = append(argv, "--effort", e)
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
	return append(argv, "-p=")
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
func runAgy(ctx context.Context, r agyRequest, sink agent.Sink) (agent.Result, error) {
	bin, err := paths.AgentBin("agy")
	if err != nil {
		return agent.Result{}, fmt.Errorf("gemini: %w", err)
	}
	stdin, err := agyStdin(r)
	if err != nil {
		return agent.Result{}, err
	}
	cmd, tail := agent.ChildCmd(ctx, bin, agyArgv(r), r.Env, r.Cwd, stdin, r.KeepEnv, true)
	return agent.RunEventStream(ctx, "gemini", cmd, tail, sink, parseAgyStream, nil)
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
func parseAgyStream(br *bufio.Reader, sink agent.Sink, onMeaningful func(), logs ...agent.StreamLog) (agent.Result, error) {
	return parseAgyEvents(br, sink, false, agent.PickLog(logs))
}

// parseAgyEvents stops at the first result when once is set, preserving buffered lines for the next turn.
func parseAgyEvents(br *bufio.Reader, sink agent.Sink, once bool, lg agent.StreamLog) (res agent.Result, _ error) {
	ts := agent.TransportTracker{Start: agent.Clock()}
	t := &agyTurn{sink: sink, lg: lg, once: once, res: &res}
	defer func() { res.Transport = ts.Result() }()
	err := agent.DecodeLines(br, lg, func(raw []byte) (bool, error) {
		var ev agyEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.flush()
			_ = agent.EmitError(lg, sink, "decode agy stream-json: "+err.Error()) // the decode error aborts
			return false, fmt.Errorf("decode agy stream-json: %w", err)
		}
		ts.Event(agent.Clock())
		return t.handle(ev)
	})
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
	sink      agent.Sink
	lg        agent.StreamLog
	once      bool
	res       *agent.Result
	ans       agent.Answer
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
	if err := applyAgyResult(t.res, ev); err != nil {
		_ = agent.EmitError(t.lg, t.sink, err.Error()) // the agy error returned below aborts
		if t.once {
			err = agent.NewResultError(err) // the result was consumed: the turn failed, the stream is at a boundary
		}
		return false, err
	}
	t.lg.Emit("result", cmp.Or(t.response, t.ans.String()))
	return t.once, nil
}

// emitAgyToolStart marks progress once per tool step: agy runs tools without tool events, so none can be shown.
func emitAgyToolStart(ev agyEvent, toolOpen *bool, sink agent.Sink) error {
	if ev.StepUpdate.StepType != agyToolStep {
		return nil
	}
	open := ev.StepUpdate.State != "DONE"
	start := open && !*toolOpen
	*toolOpen = open
	if !start {
		return nil
	}
	return sink.Emit(agent.Event{Type: agent.EventProgress})
}

// noteAgyStep tracks the unfinished non-response step and logs each new step type/state once.
func noteAgyStep(ev agyEvent, lastStep, openStep *string, lg agent.StreamLog) {
	if t, st := ev.StepUpdate.StepType, ev.StepUpdate.State; t != "agent_response" && t != "user_input" && st != "" {
		if st != "DONE" {
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

// finishAgyTurn flushes the pending log line and sets the answer; a run whose text arrived only in
// result.response (no agent_response deltas) still answers.
func finishAgyTurn(res agent.Result, sink agent.Sink, ans *agent.Answer, response string, flush func(), openStep string, once bool) (agent.Result, error) {
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
		return res, &agent.Error{Kind: agent.KindFatal, Agent: "gemini", Progressed: true, Err: errors.New("agy: partial result: " + why)}
	}
	return res, nil
}

func applyAgyResult(res *agent.Result, ev agyEvent) error {
	out := ev.Result
	if out.ConversationID != "" {
		res.SessionID = out.ConversationID
	}
	res.Usage = agent.Usage{
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

// --- gemini: report-only. agy (the agent's binary, name unchanged) has no known settings-file
// auth-type knob like the old gemini-cli's security.auth.selectedType, so there is nothing to render;
// auth is the cached Google login marker at launch time (see agent.GeminiLoginPresent).

func reportGemini(sel providers.Selection, out io.Writer) (bool, error) {
	if sel.Provider.Auth.Method() == providers.AuthCLI {
		fmt.Fprintln(out, "gemini: skipped: session provider is passthrough, not keyed")
		return false, nil
	}
	fmt.Fprintln(out, "gemini: no config render; agy has no settings-auth equivalent (launch-time env/login only)")
	return false, nil
}

// renderGeminiSettings merges the spec's exact-match guard denies into agy's
// ~/.gemini/antigravity-cli/settings.json permissions.deny, preserving every other key.
func renderGeminiSettings(home string, o render.RenderOptions, out io.Writer) (bool, error) {
	cfg, err := agentconfig.LoadConfig()
	if err != nil {
		fmt.Fprintf(out, "gemini-settings: skipped: %v\n", err)
		return false, fmt.Errorf("gemini-settings: load agentconfig: %w", err)
	}
	if !cfg.Agent("gemini").Permission.UseGuardRules {
		return false, nil
	}
	var rules []string
	for _, r := range cfg.Guard().Deny {
		cmd, ok := strings.CutPrefix(r, "Bash(")
		cmd, ok2 := strings.CutSuffix(cmd, ")")
		if !ok || !ok2 || strings.HasSuffix(cmd, ":*") {
			fmt.Fprintf(out, "gemini-settings: note: %q is not exact-match; agy denies exact commands only, skipped\n", r)
			continue
		}
		rules = append(rules, "command("+cmd+")")
	}
	path := filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
	raw, readErr := os.ReadFile(path)
	existed := readErr == nil
	if len(rules) == 0 && !existed {
		return false, nil // nothing to deny: don't create agy's settings.json just to write an empty list
	}
	next := map[string]any{}
	if existed {
		if err := json.Unmarshal(raw, &next); err != nil {
			fmt.Fprintf(out, "gemini-settings: skipped: %s is not plain JSON: %v\n", path, err)
			return false, fmt.Errorf("gemini-settings: %s is not plain JSON: %w", path, err)
		}
		if next == nil { // "null" decodes without error but leaves next nil, not an empty object
			fmt.Fprintf(out, "gemini-settings: skipped: %s is not plain JSON: not an object\n", path)
			return false, fmt.Errorf("gemini-settings: %s is not plain JSON: not an object", path)
		}
	}
	perms, _ := next["permissions"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
	}
	existing, _ := perms["deny"].([]any)
	seen := map[string]bool{}
	merged := make([]string, 0, len(existing)+len(rules))
	for _, e := range existing {
		s, ok := e.(string)
		if !ok || seen[s] {
			continue
		}
		seen[s] = true
		merged = append(merged, s)
	}
	for _, r := range rules {
		if seen[r] {
			continue
		}
		seen[r] = true
		merged = append(merged, r)
	}
	perms["deny"] = merged
	next["permissions"] = perms
	body, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return false, fmt.Errorf("gemini-settings: %w", err)
	}
	return render.CommitFile(path, raw, append(body, '\n'), existed, o, out, "gemini-settings")
}

// geminiRouteDetail reports agy's own GEMINI_MODEL pin (effort rides the id's own suffix) — unset
// means agy's own default (AgyDefaultModel).
func geminiRouteDetail() string {
	model, source := providers.GeminiModel()
	return fmt.Sprintf("model=%s source=%s", cmp.Or(model, "(default)"), source)
}

// agyModel is the --model agy runs for req on t; a Databricks row gets the id its gateway mode lists.
func agyModel(req agent.RunOptions, t providers.Target) string {
	if req.Model == "" || t.Model == "" { // the slot table's opus id isn't agy's own: GEMINI_MODEL pin, else flash-high
		t.Model = AgyDefaultModel
	}
	gt, _ := agent.ResolveAgentModel(agent.ModelAsk{Agent: "gemini", Flag: req.Model, Target: t, Applied: true})
	if agent.GeminiGatewayRow(t.Provider) {
		_, arg := agyGatewayArg(gt.Model)
		return arg
	}
	return gt.Model
}

// newAgyRequest is the agy run for req: only the resolved credential env carries over; agyArgv builds the argv.
func newAgyRequest(p agent.CLIPrep, req agent.RunOptions) agyRequest {
	return agyRequest{
		Prompt: p.Prompt, Model: agyModel(req, p.Target), Effort: req.Effort, Cwd: req.WorkDir, Tools: req.Tools, Autonomous: req.Autonomous,
		DebugFile: req.DebugFile, Env: p.Env, KeepEnv: req.KeepEnv, SessionID: req.SessionID, OutputSchema: req.OutputSchema,
	}
}

// agyLive holds `agy --input-format stream-json`: one conversation, one NDJSON line per turn.
type agyLive struct {
	tools   bool
	intro   string // emulated system prompts, owed to the first turn only
	resumed bool   // a --session conversation: agy's cumulative usage already holds its history
	prev    agent.Usage
}

func (b *agyLive) Build(ctx context.Context, req agent.RunOptions) (*exec.Cmd, *agent.TailBuffer, string, error) {
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
	b.tools, b.resumed = areq.Tools, req.SessionID != ""
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
func (b *agyLive) Parse(dec *bufio.Reader, sink agent.Sink, lg func(kind, text string)) (agent.Result, error) {
	res, err := parseAgyEvents(dec, sink, true, lg)
	total := res.Usage
	if b.resumed { // the history behind a --session is unknown: this turn reports no usage, later ones their delta
		b.resumed, b.prev, res.Usage = false, total, agent.Usage{}
		return res, err
	}
	res.Usage = agent.Usage{InputTokens: max(total.InputTokens-b.prev.InputTokens, 0), OutputTokens: max(total.OutputTokens-b.prev.OutputTokens, 0),
		CacheReadTokens: max(total.CacheReadTokens-b.prev.CacheReadTokens, 0), CostUSD: max(total.CostUSD-b.prev.CostUSD, 0)}
	b.prev = total
	return res, err
}
