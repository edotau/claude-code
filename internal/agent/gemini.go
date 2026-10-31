package agent

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
)

type geminiEngine struct{ baseEngine }

var _ headlessEngine = geminiEngine{}

func (geminiEngine) attach(prompt string, paths []string, tools bool) (string, error) {
	if tools {
		return attachSuffix(prompt, paths), nil
	}
	return attachInline(prompt, paths)
}

func (geminiEngine) before(req RunOptions, t providers.Target) providers.Target {
	t.Model = agyModel(req, t)
	return t
}

func (geminiEngine) exec(ctx context.Context, p cliPrep, req RunOptions, sink Sink) (Result, error) {
	areq := p.agyRequest(req)
	res := p.res
	res.Model = areq.Model
	out, err := runAgy(ctx, areq, sink)
	res.Answer, res.Usage, res.Transport, res.Structured = out.Answer, out.Usage, out.Transport, out.Structured
	if out.SessionID != "" {
		res.SessionID = out.SessionID
	}
	return res, err
}

func (geminiEngine) login(_ *providers.Provider, model string, h *Headless) ([]string, []string, error) {
	return geminiLogin(model, h)
}

// wire serves only a gateway row: Serves refuses agy every other non-login row.
func (geminiEngine) wire(ctx context.Context, r wireRoute, h *Headless) ([]string, []string, error) {
	if isDirect(ctx) { // --direct skips the router that swaps the Databricks token
		return nil, nil, fmt.Errorf("gemini: --direct has no Databricks route for agy (profile %s needs the router's token swap)", r.Provider.Name)
	}
	return geminiGateway(ctx, r.Provider, r.Target.Model, h) // always routed: the router fixes agy's requests for Databricks
}

func (e geminiEngine) fallbackTarget(reg *providers.Registry) (providers.Target, string, bool) {
	if p, ok := loginProvider(reg, "google", e.name); ok && GeminiLoginPresent() {
		// the GEMINI_MODEL pin beats the registry's bare slot id
		t, _ := ResolveAgentModel(ModelAsk{Agent: e.name, Target: providers.Target{Provider: p, Model: p.Models[models.Opus]}})
		return t, "gemini (Google login via agy)", true
	}
	return providers.Target{}, "", false
}

func (geminiEngine) LoginStatus() (bool, string) {
	return GeminiLoginPresent(), "Google login (agy)"
}

// followsPin keeps agy on its Google login unless GEMINI_PROFILE names the session's Databricks row.
func (geminiEngine) followsPin(p *providers.Provider) bool {
	return !GeminiGatewayRow(p) || providers.Pin(providers.PinGeminiProfile) == p.Name
}

// noRoute refuses a Databricks gateway row that no GEMINI_PROFILE pin selected: never silently.
func (geminiEngine) noRoute(_ *providers.Registry, t providers.Target) error {
	if GeminiGatewayRow(t.Provider) {
		return errors.New("gemini: agy needs a Google login (agy auth login) or a GEMINI_PROFILE pin")
	}
	return nil
}

func (geminiEngine) checkLogin(context.Context) (providers.AuthVerdict, string) {
	if geminiPresent() {
		return providers.VerdictOK, "agy login present"
	}
	return providers.VerdictNeedsLogin, ""
}

func (geminiEngine) render(_ *providers.Registry, sel providers.Selection, home string, o RenderOptions, out io.Writer) (bool, error) {
	d, err := reportGemini(sel, out)
	if err != nil {
		return d, err
	}
	gd, err := renderGeminiSettings(home, o, out)
	return d || gd, err
}

func (geminiEngine) configPaths(home string) ([]labelPath, error) {
	home, err := homeOr(home)
	if err != nil {
		return nil, err
	}
	return []labelPath{{"gemini-settings", filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")}}, nil
}

func (geminiEngine) routeDetail() (string, bool) { return geminiRouteDetail(), true }

func (geminiEngine) newLive() liveBackend { return &agyLive{} }

// googleHost is Gemini's own API host; a gemini-dialect route anywhere else is a gateway.
const googleHost = "generativelanguage.googleapis.com"

func isGateway(base string) bool {
	u, err := url.Parse(base)
	return err == nil && u.Host != googleHost
}

// geminiLogin runs agy on its own cached Google sign-in (AgyLoginMarker); without one it errors with the sign-in hint.
func geminiLogin(model string, h *Headless) ([]string, []string, error) {
	if !GeminiLoginPresent() {
		s, _ := LookupSchema("gemini")
		return nil, nil, errors.New("gemini: agy is not signed in; " + s.CLI.ConfigureHint)
	}
	if h != nil {
		return nil, nil, nil // headless argv is agent.RunAgy's (prompt over stdin stream-json)
	}
	return nil, []string{"--model", model}, nil
}

// GeminiLoginPresent reports whether agy's cached Google login is on disk; agy itself owns and refreshes the token.
func GeminiLoginPresent() bool {
	home, _ := os.UserHomeDir()
	for _, p := range agyLoginMarkers(home) {
		if fi, err := os.Stat(p); err == nil && fi.Size() > 0 {
			return true
		}
	}
	return false
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

// AgyDefaultModel runs agy when the caller names no model: fast, and enough for headless legs.
const AgyDefaultModel = "gemini-3.8-flash-high"

// agyEfforts are the effort suffixes agy bakes into its model ids (gemini-3.8-flash-high).
var agyEfforts = []string{"low", "medium", "high", "xhigh", "max"}

// agyGatewayArg returns the dotted catalog id agy lists for model (effort suffix dropped) and the --model agy takes in
// gateway mode (base plus model's effort suffix); a non-Gemini id (a Databricks Claude slot) falls back to the
// GEMINI_MODEL pin, else AgyDefaultModel.
func agyGatewayArg(model string) (base, arg string) {
	base, effort, ok := agyGatewayBase(model)
	if !ok {
		base, effort, _ = agyGatewayBase(cmp.Or(providers.Pin(providers.PinGeminiModel), AgyDefaultModel))
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

// geminiGateway runs agy in its env-only gateway mode against the router's /p/<profile>/gemini route, which swaps
// in a fresh Databricks token per request; the key agy holds is a session token scoped to this profile.
func geminiGateway(ctx context.Context, p *providers.Provider, model string, h *Headless) ([]string, []string, error) {
	rb, err := ensureRouter(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("gemini: router: %w", err)
	}
	tok, err := mintSession(ctx, p.Name, os.Getpid())
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
func runAgy(ctx context.Context, r agyRequest, sink Sink) (Result, error) {
	bin, err := paths.VendorBin("agy")
	if err != nil {
		return Result{}, fmt.Errorf("gemini: %w", err)
	}
	stdin, err := agyStdin(r)
	if err != nil {
		return Result{}, err
	}
	cmd, tail := childCmd(ctx, bin, agyArgv(r), r.Env, r.Cwd, stdin, r.KeepEnv, true)
	return runEventStream(ctx, "gemini", cmd, tail, sink, parseAgyStream, nil)
}

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
func parseAgyStream(br *bufio.Reader, sink Sink, onMeaningful func(), logs ...eventLog) (Result, error) {
	return parseAgyEvents(br, sink, false, pickLog(logs))
}

// parseAgyEvents stops at the first result when once is set, preserving buffered lines for the next turn.
func parseAgyEvents(br *bufio.Reader, sink Sink, once bool, lg eventLog) (res Result, _ error) {
	ts := transportTracker{start: clock()}
	defer func() { res.Transport = ts.timing() }()
	var answer, pending strings.Builder
	flush := func() {
		lg.emit("assistant", pending.String())
		pending.Reset()
	}
	var response string
	var sawResult bool
	var openStep string // the last non-response step still running (a backgrounded command), else ""
	err := decodeLines(br, lg, func(raw []byte) (bool, error) {
		var ev agyEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			flush()
			lg.emit("error", "decode agy stream-json: "+err.Error())
			_ = sink.Emit(Event{Type: EventError, Text: "decode agy stream-json: " + err.Error()}) // the decode error aborts
			return false, fmt.Errorf("decode agy stream-json: %w", err)
		}
		ts.event(clock())
		switch ev.Event {
		case "init":
			res.SessionID = ev.ConvID
		case "step_update":
			if t, st := ev.StepUpdate.StepType, ev.StepUpdate.State; t != "agent_response" && t != "user_input" && st != "" {
				if st != "DONE" {
					openStep = t
				} else if openStep == t {
					openStep = ""
				}
			}
			if ev.StepUpdate.StepType != "agent_response" || ev.StepUpdate.TextDelta == "" {
				return false, nil
			}
			if err := sink.Emit(Event{Type: EventText, Text: ev.StepUpdate.TextDelta}); err != nil {
				return false, err
			}
			answer.WriteString(ev.StepUpdate.TextDelta)
			pending.WriteString(ev.StepUpdate.TextDelta)
		case "result":
			sawResult = true
			response = ev.Result.Response
			flush()
			if err := applyAgyResult(&res, ev); err != nil {
				lg.emit("error", err.Error())
				_ = sink.Emit(Event{Type: EventError, Text: err.Error()}) // the agy error returned below aborts
				res.Answer = strings.TrimSpace(answer.String())
				if once {
					err = resultError{err} // the result was consumed: the turn failed, the stream is at a boundary
				}
				return false, err
			}
			lg.emit("result", cmp.Or(response, answer.String()))
			if once {
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return res, err
	}
	if !sawResult {
		flush()
		res.Answer = strings.TrimSpace(answer.String())
		return res, errors.New("agy: stream ended without a result event")
	}
	return finishAgyTurn(res, sink, &answer, response, flush, openStep, once)
}

// finishAgyTurn flushes the pending log line and sets the answer; a run whose text arrived only in
// result.response (no agent_response deltas) still answers.
func finishAgyTurn(res Result, sink Sink, answer *strings.Builder, response string, flush func(), openStep string, once bool) (Result, error) {
	flush()
	if answer.Len() == 0 && response != "" {
		if err := sink.Emit(Event{Type: EventText, Text: response}); err != nil {
			return res, err
		}
		answer.WriteString(response)
	}
	res.Answer = strings.TrimSpace(answer.String())
	if !once && (res.Answer == "" || openStep != "") {
		why := "empty answer"
		if openStep != "" {
			why = "turn ended with an unfinished " + openStep + " step"
		}
		return res, &Error{Kind: KindFatal, Agent: "gemini", Progressed: true, Err: errors.New("agy: partial result: " + why)}
	}
	return res, nil
}

func applyAgyResult(res *Result, ev agyEvent) error {
	out := ev.Result
	if out.ConversationID != "" {
		res.SessionID = out.ConversationID
	}
	res.Usage = Usage{
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
