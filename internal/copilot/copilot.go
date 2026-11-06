package copilot

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig/render"
	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/shapes"
	"github.com/edotau/claude-code/internal/vault"
)

// copilotEngine is copilot's engine and its Engine: it drives the CLI through the SDK; tools are denied unless asked.
type copilotEngine struct{ agent.BaseEngine }

// copilotAgent is the one copilot engine, shared by the driver registry and the agent roster.
var copilotAgent = copilotEngine{agent.NewBaseEngine("copilot")}

var (
	_ agent.Driver        = copilotAgent
	_ agent.Engine        = copilotAgent
	_ agent.Authenticator = copilotEngine{}
)

func (e copilotEngine) Run(ctx context.Context, req agent.RunOptions, sink agent.Sink) (agent.Result, error) {
	ctx = agent.DirectCtx(ctx, req)
	cs := copilotSchema()
	t, own, err := cs.TargetCtx(ctx, req)
	if err != nil {
		return agent.Result{}, err
	}
	res, err := initialResult(cs, t, own)
	if err != nil {
		return res, err
	}
	opts, env, done, err := e.options(ctx, cs, req, t, own)
	defer done() // done is never nil, so an error path still releases a registry route
	if err != nil {
		return res, err
	}
	prompt, images, err := copilotPrompt(req)
	if err != nil {
		return res, copilotFatal(err)
	}
	out, err := copilotAsk(ctx, opts, prompt, images)
	lg := agent.EventLogFor(ctx)
	err = markToolRan(out, sink, err)
	if err != nil {
		lg.Emit("error", err.Error())
		if quotaHandoffEligible(own, t, err) {
			res.Usage = out.Usage // tokens the exhausted run spent still count
			return CopilotQuotaHandoff(req, res, out.SessionID, out.Text, err, CredentialValues(env, opts.GitHubToken))
		}
		return copilotPartial(res, out, err, sink)
	}
	lg.Emit("assistant", out.Text) // the SDK returns the whole text, so one line per run
	res.Model = cmp.Or(out.Model, res.Model)
	res.SessionID, res.Answer, res.Usage = out.SessionID, out.Text, out.Usage
	if strings.TrimSpace(out.Text) == "" {
		return res, agent.EmptyAnswer("copilot")
	}
	return res, sink.Emit(agent.Event{Type: agent.EventText, Text: out.Text})
}

// initialResult seeds the run result with the provider and model of target t.
func initialResult(cs agent.Schema, t providers.Target, own bool) (agent.Result, error) {
	res := agent.Result{Provider: cs.OwnProfile(), Model: models.Strip1M(t.Model)}
	if own {
		return res, nil
	}
	if t.Provider == nil {
		return res, copilotFatal(errors.New("agent target resolved to no provider"))
	}
	res.Provider = t.Provider.Name
	return res, nil
}

// quotaHandoffEligible reports whether err is a quota failure on a copilot-billed route, so the task can hand off.
func quotaHandoffEligible(own bool, t providers.Target, err error) bool {
	return (own || t.Provider.Auth.Login == "copilot") && agent.Classify("copilot", err, false).Kind == agent.KindQuota
}

// options builds the SDK options for target t; env is the BYOK route env; done (never nil) releases a registry route.
func (e copilotEngine) options(ctx context.Context, cs agent.Schema, req agent.RunOptions, t providers.Target, own bool,
) (opts SDKOptions, env []string, done func(), err error) {
	done = func() {}
	route, env, err := e.routeEnv(ctx, cs, req, t, own)
	if err != nil {
		return opts, nil, done, copilotFatal(err)
	}
	if route != nil {
		done = route.close
	}
	bin, err := e.Bin()
	if err != nil {
		return opts, nil, done, err
	}
	opts = newSDKOptions(bin, env, req, t, own)
	if route != nil {
		route.apply(&opts)
	}
	// the SDK child gets exactly BuildClientOptions' env; its base drops foreign credentials under ctx isolation
	if err := agent.CheckRouteNames(slices.Concat(opts.Env, opts.ChildEnv), req.OverlaySet); err != nil {
		return opts, nil, done, copilotFatal(err)
	}
	opts.Env = append(opts.Env, req.Env...) // --env goes last so it wins
	if err := applyCopilotToken(&opts, own, env); err != nil {
		return opts, nil, done, copilotFatal(err)
	}
	return opts, env, done, nil
}

// routeEnv prepares the registry route (own: none) or the BYOK route env for target t.
func (e copilotEngine) routeEnv(ctx context.Context, cs agent.Schema, req agent.RunOptions, t providers.Target, own bool,
) (route *copilotRegistryRoute, env []string, err error) {
	switch {
	case own:
	case copilotRegistryEligible(req.Direct, t):
		route, err = prepareCopilotRegistry(ctx, t)
	default:
		env, _, err = cs.Configure(ctx, t, "")
	}
	return route, env, err
}

// newSDKOptions builds the SDK options for one run from the CLI path, route env and request.
func newSDKOptions(bin string, env []string, req agent.RunOptions, t providers.Target, own bool) SDKOptions {
	var model string
	if own || (t.Provider != nil && t.Provider.Auth.Login != "") {
		model = copilotModelID(t.Model) // no BYOK block, so the session takes the catalog id itself ([1m] is ours)
	}
	// An explicit --effort overrides the pin; there is no per-call --context, so the pin alone decides it.
	rec := vault.GetAgentRecord("copilot")
	workDir := req.WorkDir
	if workDir == "" {
		workDir, _ = os.Getwd() // an empty root would make every scoped write fail RelInside
	}
	o := SDKOptions{
		CLIPath: bin, Env: env, KeepEnv: req.KeepEnv, Model: model,
		ReasoningEffort: cmp.Or(req.Effort, rec.Effort),
		Context:         rec.Context, WorkingDirectory: workDir,
		AllowAllTools: req.Tools, ReadOnlyTools: req.ReadOnly, Permissions: req.Permissions, UseLoggedInUser: true, Autonomous: req.Autonomous, Fleet: req.Fleet, EnableMemory: req.EnableMemory,
		SessionID: req.SessionID,
		System:    strings.TrimSpace(req.SystemPrompt + "\n\n" + req.AppendSystemPrompt),
	}
	if home, err := paths.CopilotHome(); err == nil { // the dirs the harness renders under the copilot home
		o.InstructionDirs = existingDirs(home)
		o.SkillDirs = existingDirs(filepath.Join(home, "skills"))
		o.AgentDirs = existingDirs(filepath.Join(home, "agents"))
	}
	return o
}

// copilotPrompt inlines text attachments; the SDK never resolves @<path>, so images ride as absolute file paths.
func copilotPrompt(req agent.RunOptions) (prompt string, images []string, err error) {
	images, rest := agent.SplitImages(req.Attach)
	if images, err = absPaths(images); err != nil {
		return "", nil, err
	}
	prompt, err = agent.AttachInline(req.Prompt, rest)
	return prompt, images, err
}

// markToolRan marks progress when the SDK run approved a tool, so a run that acted is not replayed.
func markToolRan(out SDKResult, sink agent.Sink, err error) error {
	if out.ToolRan {
		err = errors.Join(err, sink.Emit(agent.Event{Type: agent.EventProgress}))
	}
	return err
}

// copilotPartial keeps a failed run's partial text and session; streamed bytes mark progress, so Run does not retry.
func copilotPartial(res agent.Result, out SDKResult, err error, sink agent.Sink) (agent.Result, error) {
	res.SessionID, res.Answer, res.Usage = cmp.Or(out.SessionID, res.SessionID), out.Text, out.Usage
	if strings.TrimSpace(out.Text) != "" {
		if serr := sink.Emit(agent.Event{Type: agent.EventText, Text: out.Text}); serr != nil {
			err = errors.Join(err, serr)
		}
	}
	return res, err
}

func copilotFatal(err error) error {
	return &agent.Error{Kind: agent.KindFatal, Agent: "copilot", Err: err}
}

// copilotAsk is the SDK call, a var so tests reach the agent's result handling without a copilot binary.
var copilotAsk = SDKAsk

// applyCopilotToken puts the own-login launch token, then the harness account's env token, on the SDK's token field.
func applyCopilotToken(opts *SDKOptions, own bool, env []string) error {
	if own {
		tok, err := CopilotLaunchToken()
		if err != nil {
			return err
		}
		if tok != "" {
			opts.GitHubToken, opts.UseLoggedInUser = tok, false
		}
	}
	for _, kv := range env { // the harness's chosen account rides the SDK's own token field, not the stored login
		if tok, ok := strings.CutPrefix(kv, providers.CopilotTokenEnv+"="); ok && tok != "" {
			opts.GitHubToken, opts.UseLoggedInUser = tok, false
		}
	}
	return nil
}

// copilotModelID is id as Copilot's catalog spells it (dotted versions); [1m] is dropped, an unknown id is kept.
func copilotModelID(id string) string {
	id = models.Strip1M(id)
	if canonical, _, ok := models.Resolve(id); ok {
		return models.Wire("dotted", canonical)
	}
	return id
}

// absPaths makes each path absolute (the SDK sends them to a runtime with its own working directory).
func absPaths(paths []string) ([]string, error) {
	out := make([]string, len(paths))
	for i, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		out[i] = abs
	}
	return out, nil
}

func (copilotEngine) Login(_ *providers.Provider, model string, h *agent.Headless) (env, argv []string, err error) {
	return copilotLogin(model, h)
}

func (copilotEngine) Wire(_ context.Context, r agent.WireRoute, h *agent.Headless) (env, argv []string, err error) {
	client := providers.Target{Provider: r.Target.Provider, Model: r.Target.Model, OneM: r.Target.OneM}.ClientModel()
	return copilotWire(r.Provider, r.Dialect, r.Base, r.Model, providers.ContextWindow(r.Target.Provider, client), r.Cred, r.Routed, h)
}

func (e copilotEngine) FallbackTarget(reg *providers.Registry) (providers.Target, string, bool) {
	return agent.RouteFallback(reg, e.DescribeRoute(reg))
}

func (copilotEngine) DescribeRoute(reg *providers.Registry) agent.RouteInfo {
	return describeCopilotRoute(reg)
}

func (copilotEngine) Gate(reg *providers.Registry, t providers.Target) (providers.Target, string, error) {
	return copilotGate(reg, t)
}

// GateExplicit also turns a gate move off the pinned GitHub profile into a quota error and keeps the requested tier.
func (e copilotEngine) GateExplicit(req agent.RunOptions, reg *providers.Registry, t providers.Target) (providers.Target, string, error) {
	from := t.Provider
	t, note, err := copilotGate(reg, t)
	if err == nil && t.Provider != from && req.Provider != "" && req.Provider == vault.GetAgentRecord("copilot").Profile {
		return t, note, &agent.Error{Kind: agent.KindQuota, Agent: e.Name(), Err: errors.New(note)}
	}
	if tier := ResumeAlias(req.Model); err == nil && t.Provider != from && t.Provider.Models[tier] != "" {
		t.Model = t.Provider.Models[tier] // the fallback serves the requested tier, not its default opus
	}
	return t, note, err
}

func (e copilotEngine) NoRoute(reg *providers.Registry, _ providers.Target) error {
	return agent.RouteErr(e.DescribeRoute(reg))
}

// FallbackNote keeps the Databricks fallback's own notice: the gate already names the account and the profile.
func (copilotEngine) FallbackNote(ft providers.Target, note string) (string, bool) {
	return note, ft.Provider != nil && ft.Provider.Preset == providers.PresetDatabricks
}

func (copilotEngine) Presence(router.Cell) error {
	if u := copilotPinLogin(); u != "" && !slices.Contains(GHAccounts(), u) {
		return fmt.Errorf("pinned gh user %s is not logged in (gh auth login); no other account is used", u)
	}
	return nil
}

// Verify is copilot's config.json verdict (CopilotStoredLogin), not AuthStatus, so provider rows print as before.
func (copilotEngine) Verify(context.Context) (providers.AuthVerdict, string) {
	if l := CopilotStoredLogin(); l != "" {
		return providers.VerdictOK, "signed in as " + l
	}
	return providers.VerdictNeedsLogin, ""
}

func (copilotEngine) Render(reg *providers.Registry, sel providers.Selection, home string, o render.RenderOptions, out io.Writer) (bool, error) {
	return renderCopilot(reg, sel, home, o, out)
}

func (copilotEngine) ConfigPaths(home string) ([]render.LabelPath, error) {
	home, err := render.HomeOr(home)
	if err != nil {
		return nil, err
	}
	return []render.LabelPath{
		{Label: "copilot-hooks", Path: filepath.Join(home, ".copilot", "hooks", "claude-code-harness.json")},
		{Label: "copilot-settings", Path: filepath.Join(home, ".copilot", "settings.json")},
	}, nil
}

func (copilotEngine) NativeRegistry(ctx context.Context, t providers.Target) bool {
	return UsesCopilotRegistry(ctx, t)
}

func (copilotEngine) RegistryLaunch(
	ctx context.Context, reg *providers.Registry, t providers.Target, args []string, dry bool,
) (agent.RegistryLaunch, error) {
	return ConfigureCopilotRegistry(ctx, reg, t, args, dry)
}

func (copilotEngine) RouteDetail() (string, bool) {
	reg, _, selErr, ok := agent.LoadSelection()
	if !ok {
		return "", false
	}
	return copilotRouteDetail(reg, selErr), true
}

// copilotSchema is the registered copilot roster row.
func copilotSchema() agent.Schema {
	s, _ := agent.LookupSchema("copilot")
	return s
}

func init() {
	agent.Register(agent.Agent{Row: row, Driver: copilotAgent})
}

// row is copilot's roster entry: binary, dialects, capabilities, events and launch hints.
var row = agent.Schema{Name: "copilot", CLI: agent.CLI{
	PromptVia: agent.PromptMessage,
	Binary:    "copilot", Dialects: []string{providers.ProtocolAnthropic, providers.ProtocolOpenAI},
	RouterSuffix: map[string]string{providers.ProtocolAnthropic: "", providers.ProtocolOpenAI: "/" + providers.ProtocolOpenAI,
		providers.ProtocolResponses: "/" + providers.ProtocolResponses},
	MinVersion: MinMultiProviderCLI, MinReason: "named-provider model registry needs it",
	Caps: agent.Caps{Model: agent.Native, Provider: agent.Native, Effort: agent.Native, WorkDir: agent.Native, SessionID: agent.Native,
		Tools: agent.Native, ReadOnly: agent.Native, Autonomous: agent.Native, Fleet: agent.Native, Memory: agent.Native, Direct: agent.Native, Env: agent.Native, Permissions: agent.Native,
		SystemPrompt: agent.Native, AppendSystemPrompt: agent.Native, OutputSchema: agent.Emulated, Persona: agent.Emulated},
	Events:       []string{agent.EventStart, agent.EventText, agent.EventProgress, agent.EventResult, agent.EventError},
	ResumeFormat: "copilot --resume %s", ModelsProvider: "github", ReadNeedsTools: true,
	LoginArgv:     []string{"copilot", "login"},
	ConfigureHint: "run `claude-code configure github` (Copilot) or set an Anthropic/Gemini key",
}}

// Seams for the mid-dispatch quota hand-off; tests replace them.
var handoffDir = func(root string) string { return memstore.HandoffsDir(memstore.Dir(root)) }

// CopilotQuotaHandoff marks the exhausted account and returns a redacted handoff for the next fallback dispatch;
// partialSession and partialText are what the failed run got out.
func CopilotQuotaHandoff(
	req agent.RunOptions, res agent.Result, partialSession, partialText string, cause error, creds []string,
) (agent.Result, error) {
	acct := agent.NoteCopilotQuota(cause)
	res.SessionID = cmp.Or(partialSession, res.SessionID)
	path, _, werr := writeCopilotHandoff(req, res, partialText, acct, cause, creds)
	if werr != nil {
		fmt.Fprintf(os.Stderr, "copilot: quota exhausted mid-task; handoff not written: %v\n", werr)
		return res, cause
	}
	res.Handoff = path
	return res, cause
}

// ResumeAlias is model's alias (bare, "<profile>:<alias>" or a Claude id's family) for the fallback; else "" (its opus).
func ResumeAlias(model string) string {
	if _, alias, ok := strings.Cut(model, ":"); ok {
		model = alias
	}
	if slices.Contains(models.AliasNames, model) {
		return model
	}
	canon, _ := models.Canonical(model)
	return models.FamilyOf(canon)
}

// writeCopilotHandoff writes memory/<repo>/handoffs/<date>-copilot-<shortid>[-N].md, redacted, and returns path + body.
func writeCopilotHandoff(
	req agent.RunOptions, res agent.Result, partial, acct string, cause error, creds []string,
) (string, string, error) {
	root := req.WorkDir
	if root == "" {
		root, _ = os.Getwd()
	}
	session := "none exposed"
	if res.SessionID != "" {
		session = fmt.Sprintf("%s (`copilot --resume %s`)", res.SessionID, res.SessionID)
	}
	doc := fmt.Sprintf("# Copilot hand-off — quota exhausted mid-task\n\n"+
		"- date: %s\n- account: %s\n- provider: %s\n- model: %s\n- workdir: %s\n- session: %s\n- reason: %s\n\n"+
		"## Task\n\n%s\n\n## Partial answer\n\n%s\n",
		time.Now().UTC().Format(time.RFC3339), cmp.Or(acct, "unpinned"), cmp.Or(res.Provider, "?"),
		cmp.Or(res.Model, "?"), root, session, agent.FallbackReason(cause),
		strings.TrimSpace(req.Prompt), cmp.Or(strings.TrimSpace(partial), "(none)"))
	doc = shapes.Redact(redactValues(doc, creds))
	dir := handoffDir(root)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	path, err := createExclusive(dir, time.Now().Format("2006-01-02")+"-copilot-"+shortID(res.SessionID), []byte(doc))
	if err != nil {
		return "", "", err
	}
	return path, doc, nil
}

// redactValues masks every resolved credential value (8+ chars) in s, whatever its shape.
func redactValues(s string, creds []string) string {
	for _, v := range creds {
		if len(v) >= 8 {
			s = strings.ReplaceAll(s, v, "REDACTED")
		}
	}
	return s
}

// CredentialValues are the dispatch's resolved secrets: the SDK token plus every *_TOKEN/*_KEY/*_SECRET env value.
func CredentialValues(env []string, token string) []string {
	vals := []string{token}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if v != "" && (strings.HasSuffix(k, "_TOKEN") || strings.HasSuffix(k, "_KEY") || strings.HasSuffix(k, "_SECRET")) {
			vals = append(vals, v)
		}
	}
	return vals
}

// createExclusive writes body to a temp file, then hard-links it to <base>.md, or <base>-2.md, … — never overwriting.
func createExclusive(dir, base string, body []byte) (string, error) {
	tmp, err := os.CreateTemp(dir, ".handoff-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	for n := 1; n <= 100; n++ {
		path := filepath.Join(dir, base+".md")
		if n > 1 {
			path = filepath.Join(dir, fmt.Sprintf("%s-%d.md", base, n))
		}
		err := os.Link(tmp.Name(), path)
		if err == nil {
			return path, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("handoff: 100 files named %s already exist", base)
}

// shortID is the session id's first 8 characters, else 8 random hex digits.
func shortID(session string) string {
	if s := strings.ReplaceAll(session, "-", ""); len(s) >= 8 {
		return s[:8]
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
