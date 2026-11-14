package codex

import (
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
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentid"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/textutil"
)

type codexEngine struct{ agent.BaseEngine }

var _ agent.HeadlessDriver = codexEngine{}

func (codexEngine) Attach(prompt string, paths []string, _ bool) (string, error) {
	_, rest := agent.SplitImages(paths)
	return agent.AttachInline(prompt, rest)
}

func (e codexEngine) Exec(ctx context.Context, p agent.CLIPrep, req agent.RunOptions, sink wire.Sink) (wire.Result, error) {
	res, prompt, env, argv := p.Result, p.Prompt, p.Env, p.Argv
	res.Emulated = append(res.Emulated, p.Emulated...)
	bin, err := e.Bin()
	if err != nil {
		return res, err
	}
	extra := codexInstructionArgs(req)
	if req.OutputSchema != "" {
		file, err := codexSchemaFile(req.OutputSchema)
		if err != nil {
			return res, retry.Fatal(e.Name(), err)
		}
		defer func() { _ = os.Remove(file) }()
		extra = append(extra, "--output-schema", file)
	}
	codexJSON := codexHasJSON()
	if codexJSON {
		extra = append(extra, "--json")
	}
	argv = codexExecArgs(codexImageArgs(argv, req.Attach), extra...)
	cmd, tail := agent.ChildCmd(ctx, bin, argv, env, req.WorkDir, prompt, req.KeepEnv, true)
	if codexJSON {
		sandbox, _ := codexSandbox(ctx, req.Tools) // RenderSettings refused a bad value; read-only here is the strictest reading
		sink = &denialSink{next: sink, p: req.Permissions, tools: req.Tools, sandbox: sandbox, commands: map[string]string{}}
		return agent.RunStream(ctx, e.Name(), cmd, tail, res, sink, parseCodexStream)
	}
	var answer strings.Builder
	var banner agent.HeadBuffer
	cmd.Stderr = io.MultiWriter(cmd.Stderr, &banner)
	cmd.Stdout = wire.TeeWriter(ctx, io.MultiWriter(wire.SinkWriter(sink), &answer))
	agentconfig.RecordOverlayDebug(e.Name(), cmd.Env)
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

// RenderSettings sets the effort on a routed launch, then the default launch env and args, then (headless) the policy.
func (e codexEngine) RenderSettings(ctx context.Context, s agent.Settings, env, argv []string) ([]string, []string, []string, func(), error) {
	argv, emulated, err := codexEffortArgv(s, argv)
	if err != nil {
		return env, argv, emulated, func() {}, err
	}
	env, argv, base, cleanup, err := e.BaseEngine.RenderSettings(ctx, s, env, argv)
	if emulated = append(emulated, base...); err != nil || !s.Headless {
		return env, argv, emulated, cleanup, err
	}
	sandbox, err := codexSandbox(ctx, s.Tools)
	if err != nil {
		return env, argv, emulated, cleanup, err
	}
	req := agent.RunOptions{Permissions: s.Permissions, Tools: s.Tools, SafeMode: s.SafeMode}
	argv, env, remove, err := codexApplyPermissions(req, argv, env)
	if err != nil {
		return env, argv, emulated, func() { remove(); cleanup() }, err
	}
	env, notes, rmCache, err := codexToolsEnv(ctx, env, s.Tools, sandbox)
	return env, argv, append(emulated, notes...), func() { rmCache(); remove(); cleanup() }, err
}

// codexEffortArgv adds s.Effort, in codex's spelling, before exec's own flags; a plain launch has no route to tune.
func codexEffortArgv(s agent.Settings, argv []string) ([]string, []string, error) {
	if s.Effort == "" || !slices.ContainsFunc(argv, func(a string) bool { return strings.HasPrefix(a, "model_provider=") }) {
		return argv, nil, nil
	}
	native, _ := agentid.NativeEffort(agentid.Codex, agentid.Effort(s.Effort))
	if native == "" {
		return argv, nil, fmt.Errorf("codex: %w", agentid.ValidEffort(agentid.Codex, s.Effort))
	}
	var emulated []string
	if note := agent.EffortClamp(agentid.Codex, s.Effort); note != "" {
		emulated = append(emulated, note)
	}
	at := len(argv)
	if i := slices.Index(argv, "--skip-git-repo-check"); i >= 0 {
		at = i
	}
	return slices.Insert(slices.Clone(argv), at, "-c", "model_reasoning_effort="+agentconfig.TOMLQuote(native)), emulated, nil
}

func (codexEngine) Wire(ctx context.Context, r agent.WireRoute, h *agent.Headless) ([]string, []string, error) {
	return codex(ctx, r.Provider, r.Base, r.Model, r.Cred, h)
}

func (e codexEngine) FallbackTarget(ctx context.Context, reg *providers.Registry) (providers.Target, string, bool) {
	if p, ok := reg.LoginProfile("openai", "codex"); ok && codexChatGPTLogin() == nil {
		// Codex's own config.toml model beats the catalog; the model pin beats both.
		model := p.Models[models.Opus]
		if own := codexOwnModel(); own != "" {
			model = own
		}
		t, _ := agent.ResolveAgentModel(ctx, agent.ModelAsk{Agent: e.Name(), Target: providers.Target{Provider: p, Model: model}})
		return t, "chatgpt (codex login)", true
	}
	return providers.Target{}, "", false
}

func (codexEngine) Render(ctx context.Context, reg *providers.Registry, sel providers.Selection, _ string, o agentconfig.RenderOptions, out io.Writer) (bool, error) {
	return renderCodex(ctx, reg, sel, o, out)
}

func (codexEngine) ConfigPaths(home string) ([]agentconfig.LabelPath, error) {
	path, err := paths.CodexConfigPath(home)
	if err != nil {
		return nil, err
	}
	return []agentconfig.LabelPath{{Label: "codex", Path: path}}, nil
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

var (
	codexJSONMu    sync.Mutex
	codexJSONKnown bool
	codexJSONValue bool
)

// codexHasJSON is true when the installed codex has `exec --json`; only a definitive probe is cached per process.
func codexHasJSON() bool {
	codexJSONMu.Lock()
	defer codexJSONMu.Unlock()
	if codexJSONKnown {
		return codexJSONValue
	}
	bin, err := paths.AgentBin("codex")
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), agent.ProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "exec", "--help")
	cmd.WaitDelay = time.Second                                                        // a helper holding the pipe must not outlive the timeout
	cmd.Env = agent.EnvSpec{Mode: agent.EnvAgent}.Environ(agent.ProbeContext(ctx, "")) // no login or route credential
	out, runErr := cmd.CombinedOutput()
	has := strings.Contains(string(out), "--json")
	if ctx.Err() == nil && (runErr == nil || has) {
		codexJSONKnown, codexJSONValue = true, has
	}
	return has
}

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
	_, err = f.Write(body)
	if cerr := f.Close(); err == nil {
		err = cerr // delayed-allocation filesystems report write errors at close
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// codexStrict makes every object schema OpenAI-strict (additionalProperties false, all required); harness revalidates.
func codexStrict(v any) any {
	switch x := v.(type) {
	case map[string]any:
		codexStrictChildren(x)
		if props, ok := x["properties"].(map[string]any); ok {
			x["required"], x["additionalProperties"] = append([]string{}, slices.Sorted(maps.Keys(props))...), false // [] not null
		} else if codexObjectType(x["type"]) {
			if _, set := x["additionalProperties"]; !set { // an explicit map schema keeps its own value
				x["additionalProperties"] = false
			}
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

// RanModel is the model codex runs for t: t.Model unless the Responses swap replaced it.
func (codexEngine) RanModel(t providers.Target) string {
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
func codex(ctx context.Context, p *providers.Provider, base, model, cred string, h *agent.Headless) ([]string, []string, error) {
	model = codexResponsesModel(p, model)
	id, err := agentconfig.OwnedID(p)
	if err != nil {
		return nil, nil, err
	}
	fields := [][2]string{{"name", agentconfig.TOMLQuote(p.Name)}, {"base_url", agentconfig.TOMLQuote(base)}, {"wire_api", agentconfig.TOMLQuote("responses")}}
	env := proc.Values{}
	if p.Preset == providers.PresetDatabricks {
		env = env.Merge(proc.Values{settings.CodexProfileEnv: p.Name, settings.CodexCatalogEnv: ""})
	}
	var envHs [][2]string // env_http_headers: header name -> env var carrying its value
	if cred != "" {
		env = env.With(codexKeyEnv, cred)
		if p.Auth.HeaderStyle() == providers.HeaderXAPIKey {
			envHs = append(envHs, [2]string{agentconfig.TOMLQuote("x-api-key"), agentconfig.TOMLQuote(codexKeyEnv)})
		} else {
			fields = append(fields, [2]string{"env_key", agentconfig.TOMLQuote(codexKeyEnv)})
		}
	}
	env, envHs, fields = codexProviderHeaders(p, env, envHs, fields)
	if len(envHs) > 0 {
		fields = append(fields, [2]string{"env_http_headers", agentconfig.TOMLInline(envHs)})
	}
	argv := proc.Values{"model_provider": agentconfig.TOMLQuote(id), "model": agentconfig.TOMLQuote(model),
		"model_providers." + id: agentconfig.TOMLInline(fields)}.Args("-c")
	if path := codexCatalog(ctx, p); path != "" {
		argv = append(argv, "-c", "model_catalog_json="+agentconfig.TOMLQuote(path))
		env = env.With(settings.CodexCatalogEnv, path)
	}
	return env.Env(), codexHeadless(ctx, argv, h), nil
}

// codexHeaderEnv names the env var carrying header k's value into codex; punctuation is hex-escaped to stay distinct.
func codexHeaderEnv(k string) string {
	var b strings.Builder
	b.WriteString("CLAUDE_CODE_CODEX_HDR_")
	for i := range len(k) {
		c := k[i]
		switch {
		case c >= 'a' && c <= 'z':
			b.WriteByte(c - 32)
		case c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "_%02X", c)
		}
	}
	return b.String()
}

// Login runs codex on its own ChatGPT sign-in: the built-in openai provider, no key injected.
func (codexEngine) Login(ctx context.Context, _ *providers.Provider, model string, h *agent.Headless) ([]string, []string, error) {
	if err := codexChatGPTLogin(); err != nil {
		return nil, nil, err
	}
	return nil, codexHeadless(ctx, proc.Values{"model_provider": agentconfig.TOMLQuote("openai"), "model": agentconfig.TOMLQuote(model)}.Args("-c"), h), nil
}

// codexHome is $CODEX_HOME or ~/.codex, where codex keeps auth.json and config.toml; unresolvable is an unusable path.
func codexHome() string {
	home, err := paths.CodexHome()
	if err != nil {
		return filepath.Join(os.DevNull, ".codex") // fail closed: an absolute path no file can sit under, never the cwd
	}
	return home
}

// codexOwnModel is the top-level `model = "..."` in codex's config.toml, or "" when absent or unreadable.
func codexOwnModel() string {
	data, err := os.ReadFile(filepath.Join(codexHome(), "config.toml"))
	if err != nil {
		return ""
	}
	values, _ := readCodexStrings(string(data), func(k string) bool { return k == "model" }) // nil on a parse error
	return values["model"]
}

// codexChatGPTLogin checks the installed CLI's login status, falling back to $CODEX_HOME/auth.json if unavailable.
func codexChatGPTLogin() error {
	var probeErr error // set when the CLI could not answer: the verdict is unknown, not "not signed in"
	if bin, err := paths.AgentBin("codex"); err == nil {
		verdict, transient := codexLoginProbe(bin)
		if !transient {
			return verdict
		}
		probeErr = verdict
	}
	b, keyringNote := codexAuthJSON()
	if _, present := parseCodexTokens(b); present {
		return nil
	}
	if probeErr != nil {
		return fmt.Errorf("codex: could not verify the ChatGPT sign-in (%w); retry", probeErr)
	}
	file := filepath.Join(codexHome(), "auth.json")
	msg := fmt.Sprintf("codex: no ChatGPT sign-in in %s; run `codex login` and choose \"Sign in with ChatGPT\"", file)
	if keyringNote != "" {
		msg += fmt.Sprintf(" (keyring: %s)", keyringNote)
	}
	return errors.New(msg)
}

// codexHeadless appends the `codex exec` form when h is set; nil h is the interactive launch.
func codexHeadless(ctx context.Context, argv []string, h *agent.Headless) []string {
	if h == nil {
		return argv
	}
	sandbox, _ := codexSandbox(ctx, h.Tools) // RenderSettings refuses a bad value; the error path here is read-only
	argv = append(append([]string{"exec"}, argv...), "--skip-git-repo-check", "--sandbox", sandbox)
	if sandbox == "workspace-write" && proc.Value(ctx, codexNetworkEnv) == "1" { // opt-in: codex has no loopback-only switch, so this opens all egress
		argv = append(argv, "-c", "sandbox_workspace_write.network_access=true")
	}
	argv = append(argv, codexLeafArgs.Args("-c")...)
	if !h.Tools { // read-only shell stays; skill listings, desktop plugins and app connectors are ~30 KB per request
		argv = append(argv, codexToolsOffArgs.Args("-c")...)
	}
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

// codexLeafArgs drop what a headless leaf must not use: the sub-agent tools, request_user_input and their role text.
var codexLeafArgs = proc.Values{
	"features.multi_agent_v2.disable_native_tools": "true", "tools.experimental_request_user_input.enabled": "false",
	"features.multi_agent_v2.root_agent_usage_hint_text": `""`, "features.multi_agent_v2.multi_agent_mode_hint_text": `""`,
}

// codexToolsOffArgs drop what a tools-off exec cannot use: the skill list, desktop plugins and ChatGPT app connectors.
var codexToolsOffArgs = proc.Values{"skills.include_instructions": "false", "features.plugins": "false", "features.apps": "false"}

// codexNetworkEnv=1 opts codex tool runs into sandbox network (loopback test listeners); default sealed.
const codexNetworkEnv = "HARNESS_CODEX_NETWORK"

// codexSchema is the registered codex roster row.
func codexSchema() agent.Schema {
	s, _ := agent.LookupSchema("codex")
	return s
}

// codexKeyEnv carries the credential codex's config names (the config text names the variable, not the secret).
const codexKeyEnv = "HARNESS_CODEX_API_KEY"

func codexInstructionArgs(req agent.RunOptions) []string {
	var extra proc.Values
	if req.SystemPrompt != "" {
		extra = extra.With("instructions", agentconfig.TOMLQuote(req.SystemPrompt))
	}
	if req.AppendSystemPrompt != "" {
		extra = extra.With("developer_instructions", agentconfig.TOMLQuote(req.AppendSystemPrompt))
	}
	return extra.Args("-c")
}

// codexObjectType reports whether a JSON Schema "type" is "object", alone or in a type list.
func codexObjectType(t any) bool {
	switch v := t.(type) {
	case string:
		return v == "object"
	case []any:
		return slices.Contains(v, any("object"))
	}
	return false
}

func codexStrictChildren(x map[string]any) {
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
}

func codexProviderHeaders(p *providers.Provider, env proc.Values, envHs, fields [][2]string) (proc.Values, [][2]string, [][2]string) {
	var hs [][2]string
	for _, k := range slices.Sorted(maps.Keys(p.Headers)) {
		if !textutil.LooksSecret(p.Headers[k]) {
			hs = append(hs, [2]string{agentconfig.TOMLQuote(k), agentconfig.TOMLQuote(p.Headers[k])})
			continue
		}
		name := codexHeaderEnv(k) // A credential-shaped value rides env, never argv.
		env = env.With(name, p.Headers[k])
		envHs = append(envHs, [2]string{agentconfig.TOMLQuote(k), agentconfig.TOMLQuote(name)})
	}
	if len(hs) > 0 {
		fields = append(fields, [2]string{"http_headers", agentconfig.TOMLInline(hs)})
	}
	return env, envHs, fields
}
