package codex

import (
	"bufio"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentconfig/render"
	"github.com/edotau/claude-code/internal/vault"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
)

type codexEngine struct{ agent.BaseEngine }

var _ agent.HeadlessDriver = codexEngine{}

func (codexEngine) Attach(prompt string, paths []string, _ bool) (string, error) {
	_, rest := agent.SplitImages(paths)
	return agent.AttachInline(prompt, rest)
}

func (codexEngine) Before(req agent.RunOptions, t providers.Target) providers.Target {
	if req.Autonomous {
		fmt.Fprintln(os.Stderr, "ask: codex exec has no ask-step to disable; --tools already grants workspace-write")
	}
	return t
}

func (codexEngine) RanModel(t providers.Target) string { return codexRanModel(t) }

func (e codexEngine) Exec(ctx context.Context, p agent.CLIPrep, req agent.RunOptions, sink agent.Sink) (agent.Result, error) {
	res, prompt, env, argv := p.Result, p.Prompt, p.Env, p.Argv
	bin, err := e.Bin()
	if err != nil {
		return res, err
	}
	var extra []string
	if req.SystemPrompt != "" {
		extra = append(extra, "-c", "instructions="+agentconfig.TOMLQuote(req.SystemPrompt))
	}
	if req.AppendSystemPrompt != "" {
		extra = append(extra, "-c", "developer_instructions="+agentconfig.TOMLQuote(req.AppendSystemPrompt))
	}
	if req.OutputSchema != "" {
		file, err := codexSchemaFile(req.OutputSchema)
		if err != nil {
			return res, &agent.Error{Kind: agent.KindFatal, Agent: e.Name(), Err: err}
		}
		defer func() { _ = os.Remove(file) }()
		extra = append(extra, "--output-schema", file)
	}
	codexJSON := codexHasJSON()
	if codexJSON {
		extra = append(extra, "--json")
	}
	argv = codexExecArgs(codexImageArgs(argv, req.Attach), extra...)
	var answer strings.Builder
	var banner agent.HeadBuffer
	cmd, tail := agent.ChildCmd(ctx, bin, argv, env, req.WorkDir, prompt, req.KeepEnv, true)
	cmd.Stderr = io.MultiWriter(cmd.Stderr, &banner)
	if codexJSON {
		return agent.RunStream(ctx, e.Name(), cmd, tail, res, sink, parseCodexStream)
	}
	cmd.Stdout = agent.TeeWriter(ctx, io.MultiWriter(agent.SinkWriter(sink), &answer))
	err = cmd.Run()
	res.Answer = strings.TrimSpace(answer.String())
	if id := sessionFromBanner(banner.String()); id != "" {
		res.SessionID = id
	}
	if err == nil && res.Answer == "" {
		return res, agent.EmptyAnswer(e.Name())
	}
	return res, agent.ChildErr(ctx, e.Name(), err, tail)
}

func (codexEngine) Login(_ *providers.Provider, model string, h *agent.Headless) ([]string, []string, error) {
	return codexLogin(model, h)
}

func (codexEngine) Wire(_ context.Context, r agent.WireRoute, h *agent.Headless) ([]string, []string, error) {
	return codex(r.Provider, r.Base, r.Model, r.Cred, h)
}

func (e codexEngine) FallbackTarget(reg *providers.Registry) (providers.Target, string, bool) {
	if p, ok := agent.LoginProvider(reg, "openai", "codex"); ok && codexChatGPTLogin() == nil {
		// Codex's own config.toml model beats the catalog; the model pin beats both.
		model := p.Models[models.Opus]
		if own := codexOwnModel(); own != "" {
			model = own
		}
		t, _ := agent.ResolveAgentModel(agent.ModelAsk{Agent: e.Name(), Target: providers.Target{Provider: p, Model: model}})
		return t, "chatgpt (codex login)", true
	}
	return providers.Target{}, "", false
}

func (codexEngine) LoginStatus() (bool, string) {
	if CodexChatGPTLoginPresent() {
		return true, "ChatGPT login (codex login)"
	}
	return false, ""
}

func (codexEngine) CheckLogin(ctx context.Context) (providers.AuthVerdict, string) {
	bin, err := paths.AgentBin("codex")
	if err != nil {
		return providers.VerdictTransient, err.Error()
	}
	ctx, cancel := context.WithTimeout(ctx, agent.ProbeTimeout)
	defer cancel()
	out, err := codexStatus(ctx, bin)
	var ee *exec.ExitError
	switch {
	case err == nil:
		line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
		line, _, _ = strings.Cut(line, " - ") // drop the account/key tail codex appends
		return providers.VerdictOK, line
	case errors.As(err, &ee):
		return providers.VerdictNeedsLogin, ""
	}
	return providers.VerdictTransient, err.Error()
}

func (codexEngine) Render(reg *providers.Registry, sel providers.Selection, _ string, o render.RenderOptions, out io.Writer) (bool, error) {
	return renderCodex(reg, sel, o, out)
}

func (codexEngine) ConfigPaths(home string) ([]render.LabelPath, error) {
	path, err := paths.CodexConfigPath(home)
	if err != nil {
		return nil, err
	}
	return []render.LabelPath{{Label: "codex", Path: path}}, nil
}

// LaunchArgs prepends the -f file's codex overrides (after the harness prefix, so the file's keys win).
func (codexEngine) LaunchArgs(file string, args []string) ([]string, error) {
	if file == "" {
		return args, nil
	}
	overrides, err := codexFileOverrides(file)
	if err != nil {
		return nil, err
	}
	return append(overrides, args...), nil
}

func (codexEngine) ProfileHint(o agent.LaunchOptions) error { return codexProfileHint(o) }

func (codexEngine) RouteDetail() (string, bool) { return codexRouteDetail(), true }

// codexHasJSON is true when the installed codex has `exec --json`; checked once per process.
var codexHasJSON = sync.OnceValue(func() bool {
	bin, err := paths.AgentBin("codex")
	if err != nil {
		return false
	}
	out, _ := exec.Command(bin, "exec", "--help").CombinedOutput()
	return strings.Contains(string(out), "--json")
})

// codexExecArgs inserts extra right after the exec subcommand, where codex takes its flags.
func codexExecArgs(argv []string, extra ...string) []string {
	if len(extra) == 0 || len(argv) == 0 || argv[0] != "exec" {
		return argv
	}
	return append(append([]string{"exec"}, extra...), argv[1:]...)
}

// codexSchemaFile writes the strict form of schema to a 0600 temp file for --output-schema; remove it after the run.
func codexSchemaFile(schema string) (string, error) {
	var v any
	if err := json.Unmarshal([]byte(schema), &v); err != nil {
		return "", fmt.Errorf("--output-schema: %w", err)
	}
	body, _ := json.Marshal(codexStrict(v))
	f, err := os.CreateTemp("", "codex-schema-*.json") // CreateTemp is 0600
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Write(body); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// codexStrict makes every object schema OpenAI-strict (additionalProperties false, all properties required);
// the harness still validates the answer against the caller's original schema.
func codexStrict(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for _, k := range codexSchemaMaps {
			if m, ok := x[k].(map[string]any); ok {
				for n, c := range m {
					m[n] = codexStrict(c)
				}
			}
		}
		for _, k := range codexSubschemas {
			if c, ok := x[k]; ok {
				x[k] = codexStrict(c)
			}
		}
		if props, ok := x["properties"].(map[string]any); ok {
			req := make([]string, 0, len(props))
			for k := range props {
				req = append(req, k)
			}
			sort.Strings(req)
			x["required"], x["additionalProperties"] = req, false
		}
	case []any:
		for i, c := range x {
			x[i] = codexStrict(c)
		}
	}
	return v
}

// codexSchemaMaps hold name-to-schema maps; codexSubschemas hold one schema or a list of them. Other keys are data.
var (
	codexSchemaMaps = []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"}
	codexSubschemas = []string{"items", "prefixItems", "additionalProperties", "unevaluatedProperties", "contains",
		"not", "if", "then", "else", "anyOf", "oneOf", "allOf"}
)

// codexEvent mirrors `codex exec --json`: thread.started, item.started|completed, turn.completed|failed, error.
type codexEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Item     struct {
		ID      string `json:"id"`
		Type    string `json:"type"` // agent_message | command_execution | error | reasoning | ...
		Text    string `json:"text"`
		Command string `json:"command"`
		Status  string `json:"status"`
	} `json:"item"`
	Usage struct {
		InputTokens       int `json:"input_tokens"`
		CachedInputTokens int `json:"cached_input_tokens"`
		OutputTokens      int `json:"output_tokens"`
	} `json:"usage"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

// parseCodexStream reads `codex exec --json`: agent messages become text events (the last is the answer),
// command executions become tool events, turn.completed adds usage and turn.failed fails the run.
func parseCodexStream(br *bufio.Reader, sink agent.Sink, onMeaningful func(), logs ...agent.StreamLog) (res agent.Result, _ error) {
	ts := agent.TransportTracker{Start: agent.Clock()}
	defer func() { res.Transport = ts.Result() }()
	lg := agent.PickLog(logs)
	var last string
	var failure string
	texts := 0
	err := agent.DecodeLines(br, lg, func(raw []byte) (bool, error) {
		var ev codexEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			return false, fmt.Errorf("decode codex json: %w", err)
		}
		ts.Event(agent.Clock())
		it := ev.Item
		switch {
		case ev.Type == "thread.started":
			res.SessionID = ev.ThreadID
		case ev.Type == "item.completed" && it.Type == "agent_message" && it.Text != "":
			if onMeaningful != nil {
				onMeaningful()
			}
			if err := sink.Emit(agent.Event{Type: agent.EventText, Text: it.Text, Sep: texts > 0}); err != nil {
				return false, err
			}
			texts++
			last = it.Text
			lg.Emit("assistant", it.Text)
		case ev.Type == "item.started" && it.Type == "command_execution":
			tool := agent.ToolEvent("shell", it.ID)
			tool.Text = it.Command
			if err := sink.Emit(tool); err != nil {
				return false, err
			}
			lg.Emit("tool", it.Command)
		case ev.Type == "item.completed" && it.Type == "command_execution":
			if err := sink.Emit(agent.Event{Type: agent.EventToolResult, Tool: &agent.ToolRef{Name: "shell", ID: it.ID}, Status: it.Status}); err != nil {
				return false, err
			}
		case ev.Type == "turn.completed":
			u := ev.Usage
			res.Usage.InputTokens += u.InputTokens - u.CachedInputTokens
			res.Usage.CacheReadTokens += u.CachedInputTokens
			res.Usage.OutputTokens += u.OutputTokens
			lg.Emit("result", fmt.Sprintf("turn finished, tokens in=%d out=%d", u.InputTokens, u.OutputTokens))
		case ev.Type == "turn.failed":
			failure = cmp.Or(ev.Error.Message, "codex turn failed")
			if err := agent.EmitError(lg, sink, failure); err != nil {
				return false, err
			}
		}
		return false, nil
	})
	if err != nil {
		return res, err
	}
	res.Answer = strings.TrimSpace(last)
	if failure != "" {
		return res, errors.New(failure)
	}
	return res, nil
}

// codexResponsesModel swaps a model Responses cannot serve (the row's opus alias, gpt-oss) for the menu's first Responses model.
func codexResponsesModel(p *providers.Provider, model string) string {
	gpt := models.FamilyKind(model) == models.FamilyGPT && !models.ChatOnly(model)
	if p.Preset != providers.PresetDatabricks || gpt || providers.ServesResponses(p, model) {
		return model
	}
	return cmp.Or(providers.FirstResponsesModel(p), model)
}

// codexRanModel is the model codex runs for t: t.Model unless the Responses swap replaced it.
func codexRanModel(t providers.Target) string {
	if t.Provider == nil {
		return t.Model
	}
	wire := t.Provider.WireModel(models.Strip1M(t.Model))
	if swapped := codexResponsesModel(t.Provider, wire); swapped != wire {
		return swapped
	}
	return t.Model
}

// codex speaks only the Responses API (`wire_api = "chat"` is rejected by current builds).
func codex(p *providers.Provider, base, model, cred string, h *agent.Headless) ([]string, []string, error) {
	model = codexResponsesModel(p, model)
	id, err := agent.OwnedID(p)
	if err != nil {
		return nil, nil, err
	}
	fields := [][2]string{{"name", agentconfig.TOMLQuote(p.Name)}, {"base_url", agentconfig.TOMLQuote(base)}, {"wire_api", agentconfig.TOMLQuote("responses")}}
	var env []string
	if p.Preset == providers.PresetDatabricks {
		env = append(env, settings.CodexProfileEnv+"="+p.Name, settings.CodexCatalogEnv+"=")
	}
	if cred != "" {
		env = append(env, CodexKeyEnv+"="+cred)
		if p.Auth.HeaderStyle() == providers.HeaderXAPIKey {
			fields = append(fields, [2]string{"env_http_headers", agentconfig.TOMLInline([][2]string{{agentconfig.TOMLQuote("x-api-key"), agentconfig.TOMLQuote(CodexKeyEnv)}})})
		} else {
			fields = append(fields, [2]string{"env_key", agentconfig.TOMLQuote(CodexKeyEnv)})
		}
	}
	if len(p.Headers) > 0 {
		var hs [][2]string
		for _, k := range slices.Sorted(maps.Keys(p.Headers)) {
			hs = append(hs, [2]string{agentconfig.TOMLQuote(k), agentconfig.TOMLQuote(p.Headers[k])})
		}
		fields = append(fields, [2]string{"http_headers", agentconfig.TOMLInline(hs)})
	}
	argv := []string{"-c", "model_provider=" + agentconfig.TOMLQuote(id), "-c", "model=" + agentconfig.TOMLQuote(model),
		"-c", "model_providers." + id + "=" + agentconfig.TOMLInline(fields)}
	catalog := codexCatalogArgs(p)
	argv = append(argv, catalog...) // /model offers only ids this row serves
	if len(catalog) == 2 {
		env = append(env, settings.CodexCatalogEnv+"="+agentconfig.TOMLString(strings.TrimPrefix(catalog[1], "model_catalog_json=")))
	}
	return env, codexHeadless(argv, h), nil
}

// codexLogin runs codex on its own ChatGPT sign-in: the built-in openai provider, no key injected.
func codexLogin(model string, h *agent.Headless) ([]string, []string, error) {
	if err := codexChatGPTLogin(); err != nil {
		return nil, nil, err
	}
	argv := []string{"-c", "model_provider=" + agentconfig.TOMLQuote("openai"), "-c", "model=" + agentconfig.TOMLQuote(model)}
	return nil, codexHeadless(argv, h), nil
}

// codexHome is $CODEX_HOME or ~/.codex, where codex keeps auth.json and config.toml.
func codexHome() string {
	home, err := paths.CodexHome()
	if err != nil {
		return filepath.Join(".codex") // no resolvable home: a relative path simply misses
	}
	return home
}

// codexOwnModel is the top-level `model = "..."` in codex's config.toml, or "" (a flat scan; sections are skipped).
func codexOwnModel() string {
	data, err := os.ReadFile(filepath.Join(codexHome(), "config.toml"))
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			return "" // top-level keys end at the first table header
		}
		if k, v, ok := strings.Cut(line, "="); ok && strings.Trim(strings.TrimSpace(k), `"'`) == "model" {
			return agentconfig.TOMLString(v)
		}
	}
	return ""
}

// codexChatGPTLogin checks the installed CLI's login status, falling back to $CODEX_HOME/auth.json if unavailable.
func codexChatGPTLogin() error {
	if bin, err := paths.AgentBin("codex"); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, bin, "login", "status").CombinedOutput()
		if ctx.Err() == nil {
			text := strings.TrimSpace(string(out))
			switch {
			case err == nil && strings.Contains(strings.ToLower(text), "chatgpt"):
				return nil
			case err == nil:
				return fmt.Errorf("codex: signed in with an API key, not ChatGPT (%s); run `codex login` and choose \"Sign in with ChatGPT\"", text)
			default:
				return fmt.Errorf("codex: not signed in (%s); run `codex login` and choose \"Sign in with ChatGPT\"", text)
			}
		}
	}
	b, keyringNote := codexAuthJSON()
	var auth struct {
		Tokens json.RawMessage `json:"tokens"`
	}
	if b != nil && json.Unmarshal(b, &auth) == nil && len(auth.Tokens) > 0 && string(auth.Tokens) != "null" {
		return nil
	}
	file := filepath.Join(codexHome(), "auth.json")
	msg := fmt.Sprintf("codex: no ChatGPT sign-in in %s; run `codex login` and choose \"Sign in with ChatGPT\"", file)
	if keyringNote != "" {
		msg += fmt.Sprintf(" (keyring: %s)", keyringNote)
	}
	return errors.New(msg)
}

// codexHeadless appends the `codex exec` form when h is set; nil h is the interactive launch.
func codexHeadless(argv []string, h *agent.Headless) []string {
	var effort string
	if h != nil {
		effort = h.Effort
	}
	if effort = cmp.Or(effort, vault.GetAgentRecord("codex").Effort); effort != "" {
		argv = append(argv, "-c", "model_reasoning_effort="+agentconfig.TOMLQuote(effort))
	}
	if h == nil {
		return argv
	}
	sandbox := "read-only"
	if h.Tools {
		sandbox = "workspace-write"
	}
	argv = append(append([]string{"exec"}, argv...), "--skip-git-repo-check", "--sandbox", sandbox)
	if h.NoPersist {
		argv = append(argv, "--ephemeral")
	}
	if h.SafeMode {
		argv = append(argv, "--ignore-user-config", "--ignore-rules")
	}
	if h.SessionID != "" {
		argv = append(argv, "resume", h.SessionID)
	}
	// The prompt rides stdin behind the `-` operand: a positional prompt like "help" is a codex subcommand.
	return append(argv, "-")
}

// CodexChatGPTLoginPresent reports whether codex's own ChatGPT sign-in (~/.codex/auth.json) is on disk.
func CodexChatGPTLoginPresent() bool { return codexChatGPTLogin() == nil }

// codexKeychainService is the macOS/Linux keyring service codex stores its ChatGPT login under when
// config.toml sets cli_auth_credentials_store = "keyring" (the whole auth.json body as the secret value).
const codexKeychainService = "Codex Auth"

// codexKeychainCmd runs the OS secret-store lookup (security on macOS, secret-tool on Linux); tests
// replace it so none of this reaches the real keychain.
var codexKeychainCmd = func(ctx context.Context, args ...string) ([]byte, error) {
	name := "security"
	if runtime.GOOS == "linux" {
		name = "secret-tool"
	}
	return exec.CommandContext(ctx, name, args...).Output()
}

// codexAuthJSON is codex's ChatGPT login payload, $CODEX_HOME/auth.json first and the OS keyring second —
// codex writes there instead of the file once config.toml sets cli_auth_credentials_store = "keyring", but
// that setting is only a hint: the file is always tried first regardless of what it says. data is nil
// when neither holds a login; keyringNote is "" normally, or the reason a keyring lookup
// failed for something other than "nothing stored", for a caller that wants to say why.
func codexAuthJSON() (data []byte, keyringNote string) {
	if b, err := os.ReadFile(filepath.Join(codexHome(), "auth.json")); err == nil {
		return b, ""
	}
	if b, note := codexKeyringAuthJSON(); b != nil {
		return b, ""
	} else if note != "" {
		return nil, note
	}
	return nil, ""
}

// codexKeyringAuthJSON looks up the "Codex Auth" keyring item; an item with several accounts answers
// with the first, which is fine since codex only ever stores one ChatGPT login at a time.
func codexKeyringAuthJSON() (data []byte, note string) {
	var args []string
	switch runtime.GOOS {
	case "darwin":
		args = []string{"find-generic-password", "-s", codexKeychainService, "-w"}
	case "linux":
		args = []string{"lookup", "service", codexKeychainService}
	default:
		return nil, "" // no keyring plane wired up for this OS
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := codexKeychainCmd(ctx, args...)
	if err == nil {
		if v := strings.TrimSpace(string(out)); v != "" {
			return out, ""
		}
		return nil, "" // empty value reads as nothing stored
	}
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		switch runtime.GOOS {
		case "darwin":
			if exit.ExitCode() == 44 { // errSecItemNotFound
				return nil, ""
			}
		case "linux":
			if exit.ExitCode() == 1 && strings.TrimSpace(string(exit.Stderr)) == "" {
				return nil, "" // secret-tool's plain "not found" exit
			}
		}
		if stderr := strings.TrimSpace(string(exit.Stderr)); stderr != "" {
			return nil, stderr
		}
	}
	if ctx.Err() != nil {
		return nil, "timed out"
	}
	return nil, err.Error()
}

// CodexPlan is the ChatGPT plan codex is signed in on ("plus", "pro", "team"…), read offline from the id_token
// claim in codex's auth.json (file or keyring); "" when absent or unreadable. The token is decoded, never
// verified or printed.
func CodexPlan() string {
	b, _ := codexAuthJSON()
	if b == nil {
		return ""
	}
	var auth struct {
		Tokens struct {
			IDToken string `json:"id_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(b, &auth) != nil {
		return ""
	}
	parts := strings.Split(auth.Tokens.IDToken, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims struct {
		Auth struct {
			Plan string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return claims.Auth.Plan
}

// CodexAccessToken is codex's ChatGPT access token and account id from its auth.json (file or keyring);
// "" when absent. keyringNote is "" normally, or the reason a keyring lookup failed for something other
// than "nothing stored", for callers that want to say why no token was found.
func CodexAccessToken() (token, accountID, keyringNote string) {
	b, note := codexAuthJSON()
	if b == nil {
		return "", "", note
	}
	var auth struct {
		Tokens struct {
			Access  string `json:"access_token"`
			Account string `json:"account_id"`
		} `json:"tokens"`
	}
	if json.Unmarshal(b, &auth) != nil {
		return "", "", ""
	}
	return auth.Tokens.Access, auth.Tokens.Account, ""
}

// codexSchema is the registered codex roster row.
func codexSchema() agent.Schema {
	s, _ := agent.LookupSchema("codex")
	return s
}

// CodexKeyEnv carries the credential codex's config names (the config text names the variable, not the secret).
const CodexKeyEnv = "HARNESS_CODEX_API_KEY"
