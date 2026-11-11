package copilot

import (
	"bytes"
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

	"github.com/edotau/claude-code/internal/memory"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/textutil"
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

func (e copilotEngine) Run(ctx context.Context, req agent.RunOptions, sink wire.Sink) (wire.Result, error) {
	ctx = agent.DirectCtx(ctx, req)
	cs := copilotSchema()
	t, _, err := cs.TargetCtx(ctx, req) // copilot never resolves its own login: every target has a provider
	if err != nil {
		return wire.Result{}, err
	}
	res, err := initialResult(cs, t)
	if err != nil {
		return res, err
	}
	opts, env, done, err := e.options(ctx, cs, req, t)
	defer done() // done is never nil, so an error path still releases a registry route
	if err != nil {
		return res, err
	}
	prompt, images, err := copilotPrompt(req)
	if err != nil {
		return res, copilotFatal(err)
	}
	out, err := copilotAsk(ctx, opts, prompt, images)
	lg := wire.EventLogFor(ctx)
	err = markToolRan(out, sink, err)
	if err != nil {
		lg.Emit("error", err.Error())
		if quotaHandoffEligible(t, err) {
			res.Usage = out.Usage // tokens the exhausted run spent still count
			return copilotQuotaHandoff(req, res, out.SessionID, out.Text, err, runCredentials(opts, env, req))
		}
		return copilotPartial(res, out, err, sink)
	}
	lg.Emit("assistant", out.Text) // the SDK returns the whole text, so one line per run
	res.Model = cmp.Or(out.Model, res.Model)
	res.SessionID, res.Answer, res.Usage = out.SessionID, out.Text, out.Usage
	if strings.TrimSpace(out.Text) == "" {
		return res, agent.EmptyAnswer("copilot")
	}
	return res, sink.Emit(wire.Event{Type: wire.EventText, Text: out.Text})
}

// initialResult seeds the run result with the provider and model of target t.
func initialResult(cs agent.Schema, t providers.Target) (wire.Result, error) {
	res := wire.Result{Provider: cs.OwnProfile(), Model: models.Strip1M(t.Model)}
	if t.Provider == nil {
		return res, copilotFatal(errors.New("agent target resolved to no provider"))
	}
	res.Provider = t.Provider.Name
	return res, nil
}

// runCredentials are the run's secrets to mask: route env credentials, secret-named --env values and the registry token.
func runCredentials(opts sdkOptions, env []string, req agent.RunOptions) []string {
	creds := credentialValues(slices.Concat(env, req.Env))
	if opts.Token != nil {
		if tok, err := opts.Token(); err == nil && tok != "" {
			creds = append(creds, tok)
		}
	}
	return creds
}

// quotaHandoffEligible reports whether err is a quota failure on a copilot-billed route, so the task can hand off.
func quotaHandoffEligible(t providers.Target, err error) bool {
	return t.Provider.Auth.Login == "copilot" && retry.Classify("copilot", err, false).Kind == retry.KindQuota
}

// options builds the SDK options for target t; env is the BYOK route env; done (never nil) releases a registry route.
func (e copilotEngine) options(ctx context.Context, cs agent.Schema, req agent.RunOptions, t providers.Target,
) (opts sdkOptions, env []string, done func(), err error) {
	done = func() {}
	st, err := agent.ResolveSettings("copilot", req, t, true) // headless: a bad stored record fails before any route
	if err != nil {
		return opts, nil, done, copilotFatal(err)
	}
	route, env, err := e.routeEnv(ctx, cs, req, t)
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
	opts = newSDKOptions(bin, env, req, st)
	if route != nil {
		route.apply(&opts)
	}
	// the SDK child gets exactly buildClientOptions' env; its base drops foreign credentials under ctx isolation
	if err := agent.CheckRouteNames(slices.Concat(opts.Env, opts.ChildEnv), req.OverlaySet); err != nil {
		return opts, nil, done, copilotFatal(err)
	}
	opts.Env = append(opts.Env, req.Env...) // --env goes last so it wins
	applyCopilotToken(&opts, env)
	if route == nil && t.Provider.Auth.Login == "" && router.ArmProfile() != "" {
		armBYOK(&opts)
	}
	return opts, env, done, nil
}

// armBYOK keeps the GitHub login off an armed BYOK hop's model traffic; the CLI can still read its Keychain login.
func armBYOK(o *sdkOptions) {
	o.GitHubToken, o.UseLoggedInUser, o.ChildUnset = "", false, copilotStripEnv
}

// routeEnv prepares the registry route or the BYOK route env for target t.
func (e copilotEngine) routeEnv(ctx context.Context, cs agent.Schema, req agent.RunOptions, t providers.Target,
) (route *copilotRegistryRoute, env []string, err error) {
	if copilotRegistryEligible(req.Direct, t) {
		route, err = prepareCopilotRegistry(ctx, t)
		return route, nil, err
	}
	env, _, err = cs.Configure(ctx, t)
	return nil, env, err
}

// newSDKOptions builds the SDK options for one run from the CLI path, route env, request and resolved settings.
func newSDKOptions(bin string, env []string, req agent.RunOptions, s agent.Settings) sdkOptions {
	var model string
	if t := s.Target; t.Provider != nil && t.Provider.Auth.Login != "" {
		model = copilotModelID(t.Model) // no BYOK block, so the session takes the catalog id itself ([1m] is ours)
	}
	workDir := req.WorkDir
	if workDir == "" {
		workDir, _ = os.Getwd() // relToWorkDir needs a non-empty root
	}
	o := sdkOptions{
		CLIPath: bin, Env: env, KeepEnv: req.KeepEnv, Model: model,
		ReasoningEffort: s.Effort, // --effort over the record; there is no per-call --context, so the record decides it
		Context:         s.Record.Context, WorkingDirectory: workDir,
		AllowAllTools: req.Tools, ReadOnlyTools: req.ReadOnly, Permissions: req.Permissions, UseLoggedInUser: true, Autonomous: req.Autonomous, Fleet: req.Fleet, EnableMemory: req.EnableMemory,
		SessionID: req.SessionID,
		System:    strings.TrimSpace(req.SystemPrompt + "\n\n" + req.AppendSystemPrompt),
	}
	if home, err := paths.CopilotHome(); err == nil { // the dirs the harness renders under the copilot home
		o.InstructionDirs = existingDirs(home)
		o.SkillDirs = existingDirs(filepath.Join(home, "skills"))
		o.AgentDirs = existingDirs(filepath.Join(home, "agents"))
		o.InlineInstructions = sameInstructions(home, workDir)
	}
	return o
}

// otherInstructionFiles are the instruction sources the runtime discovers besides CLAUDE.md (workDir-relative).
var otherInstructionFiles = []string{
	"AGENTS.md", "GEMINI.md", "CLAUDE.local.md", filepath.Join(".claude", "CLAUDE.md"),
	filepath.Join(".github", "copilot-instructions.md"), filepath.Join(".github", "instructions"),
}

// sameInstructions is home's copilot-instructions.md when workDir's CLAUDE.md, its only instruction file, is already
// inside it (the runtime would load that text twice, ~7 KB per request); "" otherwise.
func sameInstructions(home, workDir string) string {
	own, err := os.ReadFile(filepath.Join(workDir, "CLAUDE.md"))
	if own = bytes.TrimSpace(own); err != nil || len(own) == 0 {
		return ""
	}
	for _, p := range append([]string{filepath.Join(home, "instructions")}, joinNames(workDir, otherInstructionFiles)...) {
		if _, err := os.Stat(p); err == nil {
			return ""
		}
	}
	rendered, err := os.ReadFile(filepath.Join(home, "copilot-instructions.md"))
	if err != nil || !bytes.Contains(rendered, own) {
		return ""
	}
	return string(rendered)
}

// joinNames joins each name onto dir.
func joinNames(dir string, names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = filepath.Join(dir, n)
	}
	return out
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
func markToolRan(out sdkResult, sink wire.Sink, err error) error {
	if out.ToolRan {
		err = errors.Join(err, sink.Emit(wire.Event{Type: wire.EventProgress}))
	}
	return err
}

// copilotPartial keeps a failed run's partial text and session; streamed bytes mark progress, so Run does not retry.
func copilotPartial(res wire.Result, out sdkResult, err error, sink wire.Sink) (wire.Result, error) {
	res.SessionID, res.Answer, res.Usage = cmp.Or(out.SessionID, res.SessionID), out.Text, out.Usage
	if strings.TrimSpace(out.Text) != "" {
		if serr := sink.Emit(wire.Event{Type: wire.EventText, Text: out.Text}); serr != nil {
			err = errors.Join(err, serr)
		}
	}
	return res, err
}

func copilotFatal(err error) error {
	return retry.Fatal("copilot", err)
}

// copilotAsk is the SDK call, a var so tests reach the agent's result handling without a copilot binary.
var copilotAsk = sdkAsk

// applyCopilotToken puts the harness account's env token on the SDK's token field.
func applyCopilotToken(opts *sdkOptions, env []string) {
	for _, kv := range env { // the harness's chosen account rides the SDK's own token field, not the stored login
		if tok, ok := strings.CutPrefix(kv, providers.CopilotTokenEnv+"="); ok && tok != "" {
			opts.GitHubToken, opts.UseLoggedInUser = tok, false
		}
	}
}

// copilotModelID is id as Copilot's catalog spells it (dotted versions); [1m] is dropped, an unknown id is kept.
func copilotModelID(id string) string {
	return models.WireOrOriginal("dotted", models.Strip1M(id))
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

func (copilotEngine) Login(p *providers.Provider, model string, h *agent.Headless) (env, argv []string, err error) {
	login := ""
	if p != nil {
		login = p.Login
	}
	return copilotLogin(model, login, h)
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
		return t, note, &retry.Error{Kind: retry.KindQuota, Agent: e.Name(), Err: errors.New(note)}
	}
	if tier := resumeAlias(req.Model); err == nil && t.Provider != from && t.Provider.Models[tier] != "" {
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
	u, err := copilotPinLoginErr()
	if err != nil {
		return fmt.Errorf("cannot read the provider registry to check the pinned gh user: %w", err)
	}
	if u != "" && !slices.Contains(GHAccounts(), u) {
		return fmt.Errorf("pinned gh user %s is not logged in (gh auth login); no other account is used", u)
	}
	return nil
}

// Verify is copilot's config.json verdict (copilotStoredLogin), not AuthStatus, so provider rows print as before.
func (copilotEngine) Verify(context.Context) (providers.AuthVerdict, string) {
	if l := copilotStoredLogin(); l != "" {
		return providers.VerdictOK, "signed in as " + l
	}
	return providers.VerdictNeedsLogin, ""
}

func (copilotEngine) Render(reg *providers.Registry, sel providers.Selection, home string, o agentconfig.RenderOptions, out io.Writer) (bool, error) {
	return renderCopilot(reg, sel, home, o, out)
}

func (copilotEngine) ConfigPaths(home string) ([]agentconfig.LabelPath, error) {
	home, err := agentconfig.HomeOr(home)
	if err != nil {
		return nil, err
	}
	return []agentconfig.LabelPath{
		{Label: "copilot-hooks", Path: hooksPath(home)},
		{Label: "copilot-settings", Path: settingsPath(home)},
	}, nil
}

func (copilotEngine) NativeRegistry(ctx context.Context, t providers.Target) bool {
	return usesCopilotRegistry(ctx, t)
}

func (copilotEngine) RegistryLaunch(
	ctx context.Context, reg *providers.Registry, t providers.Target, args []string, dry bool,
) (agent.RegistryLaunch, error) {
	return configureCopilotRegistry(ctx, reg, t, args, dry)
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
	MinVersion: minMultiProviderCLI, MinReason: "named-provider model registry needs it",
	Caps: agent.Caps{Model: agent.Native, Provider: agent.Native, Effort: agent.Native, WorkDir: agent.Native, SessionID: agent.Native,
		Tools: agent.Native, ReadOnly: agent.Native, Autonomous: agent.Native, Fleet: agent.Native, Memory: agent.Native, Direct: agent.Native, Env: agent.Native, Permissions: agent.Native,
		SystemPrompt: agent.Native, AppendSystemPrompt: agent.Native, OutputSchema: agent.Emulated, Persona: agent.Emulated},
	Events:       []string{wire.EventStart, wire.EventText, wire.EventProgress, wire.EventResult, wire.EventError},
	ResumeFormat: "copilot --resume %s", ModelsProvider: "github", ReadNeedsTools: true,
	LoginArgv:     []string{"copilot", "login"},
	ConfigureHint: "run `claude-code configure github` (Copilot) or set an Anthropic key",
	Extras:        map[string][]string{extraDefaultPermissionMode: nil}, // the CLI's own mode names, not validated here
}}

// Seams for the mid-dispatch quota hand-off; tests replace them.
var handoffDir = func(root string) string { return memory.HandoffsDir(memory.Dir(root)) }

// copilotQuotaHandoff marks the exhausted account and returns a redacted handoff for the next fallback dispatch;
// partialSession and partialText are what the failed run got out.
func copilotQuotaHandoff(
	req agent.RunOptions, res wire.Result, partialSession, partialText string, cause error, creds []string,
) (wire.Result, error) {
	acct := agent.NoteCopilotQuota(cause)
	res.SessionID = cmp.Or(partialSession, res.SessionID)
	path, werr := writeCopilotHandoff(req, res, partialText, acct, cause, creds)
	if werr != nil {
		res.Answer = partialText
		return res, errors.Join(cause, fmt.Errorf("copilot: quota exhausted mid-task; handoff not written: %w", werr))
	}
	res.Handoff = path
	return res, cause
}

// resumeAlias is model's alias (bare, "<profile>:<alias>" or a Claude id's family) for the fallback; else "" (its opus).
func resumeAlias(model string) string {
	if _, alias, ok := strings.Cut(model, ":"); ok {
		model = alias
	}
	if slices.Contains(models.AliasNames, model) {
		return model
	}
	canon, _ := models.Canonical(model)
	return models.FamilyOf(canon)
}

// writeCopilotHandoff writes memory/<repo>/handoffs/<date>-copilot-<shortid>[-N].md, redacted, and returns its path.
func writeCopilotHandoff(
	req agent.RunOptions, res wire.Result, partial, acct string, cause error, creds []string,
) (string, error) {
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
	doc = textutil.RedactSecrets(redactValues(doc, creds))
	dir := handoffDir(root)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return createExclusive(dir, time.Now().Format("2006-01-02")+"-copilot-"+shortID(res.SessionID), []byte(doc))
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

// credentialValues are the dispatch's resolved secrets: every *_TOKEN/*_KEY/*_SECRET env value, the SDK token included.
func credentialValues(env []string) []string {
	var vals []string
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
