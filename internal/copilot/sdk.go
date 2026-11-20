// The SDK box: one headless Copilot turn over the real CLI. In sdk*.go, `copilot.` is the SDK import.

package copilot

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/textutil"
	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

// defaultClientName identifies this host in the SDK's User-Agent.
const defaultClientName = "claude-code"

// denyFeedback tells the model to re-plan in prose instead of retrying a refused tool call.
const denyFeedback = "Tool use is disabled for this headless run. Answer from the prompt alone."

// sdkOptions configures one headless run. Env carries the COPILOT_PROVIDER_* BYOK block from Schema.Configure.
type sdkOptions struct {
	BudgetCredits *int
	// resolved copilot binary; empty is an error (never the SDK's bundled runtime)
	CLIPath string
	// KEY=VALUE BYOK block
	Env []string
	// names the credential scrub must not drop, from --keep-env
	KeepEnv []string
	// Copilot-catalog id; leave empty on BYOK (COPILOT_MODEL carries it)
	Model           string
	ReasoningEffort string
	EffortNotes     []string
	// ContextTier: "default" or "long_context"; empty leaves the model's own default
	Context          string
	WorkingDirectory string
	AllowAllTools    bool
	// with AllowAllTools: approve only file reads and URL fetches
	ReadOnlyTools bool
	// with AllowAllTools: Deny rejects; anything else falls to the tier
	Permissions agent.Permissions
	// always explicit: unset, the SDK silently falls back to the GitHub subscription
	UseLoggedInUser bool
	GitHubToken     string
	// MessageOptions.AgentMode = AgentModeAutopilot
	Autonomous bool
	// experimental session.RPC.Fleet.Start instead of SendAndWait
	Fleet bool
	// SessionConfig.Memory.Enabled
	EnableMemory bool
	// non-empty resumes this session instead of creating a new one
	SessionID string
	// system text, sent as SystemMessage in append mode (replace would drop the SDK guardrails)
	System string
	// InstructionDirs, SkillDirs and AgentDirs are the discovery roots the harness renders; missing ones are skipped.
	InstructionDirs, SkillDirs, AgentDirs []string
	// InlineInstructions, when set, replaces discovery of every instruction file (see sameInstructions).
	InlineInstructions string
	// Providers+Models: the multi-provider route (Model = provider/id, Token feeds each BearerTokenProvider, no login).
	Providers []copilotRegistryProvider
	Models    []copilotRegistryModel
	Token     func() (string, error)
	// ChildEnv is set on, and ChildUnset (exact names) plus ChildUnsetPrefix stripped from, the registry child's env.
	ChildEnv, ChildUnset []string
	ChildUnsetPrefix     string
}

// sdkResult is one completed turn; Model is what the runtime reports actually served it.
type sdkResult struct {
	Text      string
	SessionID string
	Model     string
	// a tool request was approved, so the run may have side effects
	ToolRan bool
	Usage   wire.Usage
	// FinishReason is the last model call's; ContentFiltered is set if any call was blocked or truncated by a filter.
	FinishReason    string
	ContentFiltered bool
}

// buildClientOptions pins stdio at an explicit CLI path; Env on the connection is the child's complete env.
func buildClientOptions(ctx context.Context, o sdkOptions) (*copilot.ClientOptions, error) {
	if strings.TrimSpace(o.CLIPath) == "" {
		return nil, errors.New("copilot CLI path is empty (the SDK would run its bundled runtime instead)")
	}
	// isolation and unset ride ctx
	base := agent.EnvSpec{Mode: agent.EnvAgent, Keep: o.KeepEnv, Agent: agentid.Copilot}.Environ(ctx)
	if o.multiProvider() {
		if err := validateRegistryOptions(o); err != nil {
			return nil, err
		}
		base = registryEnv(append(base, o.Env...), o.ChildEnv, o.ChildUnset, o.ChildUnsetPrefix)
		o.Env = nil
	}
	env := append(registryEnv(base, nil, o.ChildUnset, ""), o.Env...)
	// applied last so neither the parent's env nor --env can re-arm a git helper
	if o.ReadOnlyTools {
		env = registryEnv(env, readOnlyGitEnv, nil, "GIT_")
	}
	recorded := env
	// the SDK sets it on the child itself (copilot-sdk client.go:2282)
	if o.GitHubToken != "" {
		recorded = append(slices.Clone(env), "COPILOT_SDK_AUTH_TOKEN="+o.GitHubToken)
	}
	agentconfig.RecordOverlayDebug("copilot", recorded)
	return &copilot.ClientOptions{
		Connection:       copilot.StdioConnection{Path: o.CLIPath, Env: env},
		WorkingDirectory: o.WorkingDirectory,
		UseLoggedInUser:  new(o.UseLoggedInUser),
		GitHubToken:      o.GitHubToken,
	}, nil
}

// buildSessionConfig denies tool calls unless AllowAllTools is set.
func buildSessionConfig(o sdkOptions) *copilot.SessionConfig {
	cfg := &copilot.SessionConfig{
		ClientName:          defaultClientName,
		Model:               o.Model,
		ReasoningEffort:     o.ReasoningEffort,
		ContextTier:         copilot.ContextTier(o.Context),
		WorkingDirectory:    o.WorkingDirectory,
		Provider:            providerFromEnv(o.Env),
		OnPermissionRequest: baseHandler(o),
	}
	if o.BudgetCredits != nil {
		cfg.SessionLimits = &rpc.SessionLimitsConfig{MaxAiCredits: new(float64(*o.BudgetCredits))}
	}
	if o.EnableMemory {
		cfg.Memory = &copilot.MemoryConfiguration{Enabled: true}
	}
	cfg.SystemMessage = systemMessage(o)
	cfg.HookDirectories = hookDirectories(o)
	cfg.InstructionDirectories, cfg.SkillDirectories, cfg.AgentDirectories = o.InstructionDirs, o.SkillDirs, o.AgentDirs
	// sent once through the system prompt instead of being discovered twice
	if o.InlineInstructions != "" {
		cfg.SkipCustomInstructions, cfg.OrganizationCustomInstructions = new(true), &o.InlineInstructions
		cfg.InstructionDirectories = nil
	}
	switch {
	// every call is denied: send no tool schemas, skill or agent listings the model cannot use
	case !o.AllowAllTools:
		cfg.AvailableTools, cfg.SkillDirectories, cfg.AgentDirectories = []string{}, nil, nil
	// a headless leaf cannot fan out: no sub-agent or workflow tools, no agent listing only `task` reads
	case !o.Fleet:
		cfg.ExcludedTools, cfg.AgentDirectories = leafExcludedTools, nil
	}
	if o.multiProvider() {
		cfg.Provider = nil
		cfg.Providers, cfg.Models = namedProviders(o)
	}
	return cfg
}

// hookDirectories loads only the harness hooks dir; EnableConfigDiscovery would also pull ambient MCP and instructions.
func hookDirectories(o sdkOptions) []string {
	for _, env := range [][]string{o.ChildEnv, o.Env} {
		if h := proc.ValuesFromEnv(env)["COPILOT_HOME"]; h != "" {
			return []string{filepath.Join(h, "hooks")}
		}
	}
	h, err := paths.CopilotHome()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(h, "hooks")}
}

// leafExcludedTools are the sub-agent and workflow tools a headless leaf must not use (~20 KB of schemas per request).
var leafExcludedTools = []string{"task", "write_agent", "read_agent", "list_agents", "run_dynamic_workflow", "dynamic_workflows_manage"}

// toolOnlySections are system-prompt sections only about tools, tasks and PRs; a tools-denied run drops them (~8.6 KB).
var toolOnlySections = map[string]copilot.SectionOverride{
	copilot.SectionToolEfficiency:      {Action: copilot.SectionActionRemove},
	copilot.SectionToolInstructions:    {Action: copilot.SectionActionRemove},
	copilot.SectionRuntimeInstructions: {Action: copilot.SectionActionRemove},
}

// systemMessage appends o.System (guardrails stay), nil when empty; tools denied: customize minus toolOnlySections.
func systemMessage(o sdkOptions) *copilot.SystemMessageConfig {
	if !o.AllowAllTools {
		sections := maps.Clone(toolOnlySections)
		if strings.TrimSpace(o.System) != "" {
			sections[copilot.SectionLastInstructions] = copilot.SectionOverride{Action: copilot.SectionActionAppend, Content: "\n\n" + o.System}
		}
		return &copilot.SystemMessageConfig{Mode: "customize", Sections: sections}
	}
	if strings.TrimSpace(o.System) == "" {
		return nil
	}
	return &copilot.SystemMessageConfig{Mode: "append", Content: o.System}
}

// existingDirs keeps the entries that are directories, so an unrendered root is not handed to the runtime.
func existingDirs(dirs ...string) []string {
	var out []string
	for _, d := range dirs {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			out = append(out, d)
		}
	}
	return out
}

// buildResumeSessionConfig is buildSessionConfig's settings on client.ResumeSessionWithOptions' type.
func buildResumeSessionConfig(o sdkOptions) *copilot.ResumeSessionConfig {
	c := buildSessionConfig(o)
	return &copilot.ResumeSessionConfig{
		ClientName: c.ClientName, Model: c.Model, ReasoningEffort: c.ReasoningEffort, ContextTier: c.ContextTier,
		WorkingDirectory: c.WorkingDirectory, Provider: c.Provider, OnPermissionRequest: c.OnPermissionRequest,
		SessionLimits: c.SessionLimits, Memory: c.Memory, SystemMessage: c.SystemMessage, InstructionDirectories: c.InstructionDirectories,
		SkillDirectories: c.SkillDirectories, AgentDirectories: c.AgentDirectories, Providers: c.Providers, Models: c.Models,
		AvailableTools: c.AvailableTools, ExcludedTools: c.ExcludedTools, SkipCustomInstructions: c.SkipCustomInstructions,
		OrganizationCustomInstructions: c.OrganizationCustomInstructions,
	}
}

// providerFromEnv lifts BYOK env into the session (server mode ignores COPILOT_PROVIDER_*); last duplicate wins.
func providerFromEnv(env []string) *copilot.ProviderConfig {
	vals := proc.ValuesFromEnv(env)
	base := vals[proc.EnvCopilotProviderBaseURL]
	if base == "" {
		return nil
	}
	atoi := func(key string) int { n, _ := strconv.Atoi(vals[key]); return n }
	model := vals["COPILOT_MODEL"]
	modelID := wellKnownModel(model)
	if id := vals[proc.EnvCopilotProviderModelID]; id != "" {
		modelID = id
	}
	return &copilot.ProviderConfig{
		Type:            vals[proc.EnvCopilotProviderType],
		WireAPI:         vals[proc.EnvCopilotProviderWireAPI],
		BaseURL:         base,
		APIKey:          vals[proc.EnvCopilotProviderAPIKey],
		BearerToken:     vals[proc.EnvCopilotProviderBearerToken],
		ModelID:         modelID,
		WireModel:       model,
		MaxPromptTokens: atoi(proc.EnvCopilotProviderMaxPromptTokens),
		MaxOutputTokens: atoi(proc.EnvCopilotProviderMaxOutputTokens),
		Headers:         parseProviderHeaders(vals[proc.EnvCopilotProviderHeaders]),
	}
}

// parseProviderHeaders reverses copilotCredEnv's "Key: Value" lines joined by newlines; nil when none.
func parseProviderHeaders(raw string) map[string]string {
	var out map[string]string
	for line := range strings.SplitSeq(raw, "\n") {
		k, v, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[k] = v
	}
	return out
}

// wellKnownModel maps a gateway Claude id to copilot's form (claude-sonnet-5.5) so Claude settings apply; else as is.
func wellKnownModel(id string) string {
	canonical, _, ok := models.Resolve(id)
	if !ok || models.FamilyKind(canonical) != models.FamilyClaude {
		return id
	}
	return models.Wire("dotted", canonical)
}

// SDKModel is the catalog entry the harness prints and validates pins against.
type SDKModel struct {
	ID              string
	Name            string
	Vision          bool
	ReasoningEffort bool
	// nil when the model carries no premium billing multiplier
	Multiplier *float64
	// SupportedReasoningEfforts; empty when the model has none
	Efforts []string
	// "" when the catalog names none
	DefaultEffort string
	// MaxContextWindowTokens; 0 when the catalog reports none
	ContextTokens int
	// Policy == nil or Policy.State == "enabled"
	Enabled bool
}

// CatalogOptions is a listing's auth: a resolved account token (catalogAuth), else "" for the CLI's own login.
func CatalogOptions(cliPath, token string) sdkOptions {
	if token != "" {
		return sdkOptions{CLIPath: cliPath, GitHubToken: token}
	}
	return sdkOptions{CLIPath: cliPath, UseLoggedInUser: true}
}

// listModelsFn is the injectable seam: tests fake the copilot CLI's catalog without a real GitHub login.
var listModelsFn = func(ctx context.Context, o sdkOptions) (models []copilot.ModelInfo, err error) {
	client, stopClient, err := startClient(ctx, o, &err)
	if err != nil {
		return nil, err
	}
	defer stopClient()
	return client.ListModels(ctx)
}

// SDKListModels returns the copilot CLI's own model catalog via its logged-in GitHub session (never BYOK).
func SDKListModels(ctx context.Context, o sdkOptions) ([]SDKModel, error) {
	raw, err := listModelsFn(ctx, o)
	if err != nil {
		return nil, fmt.Errorf("list copilot models (sign in with `copilot` first, then retry): %w", err)
	}
	out := make([]SDKModel, len(raw))
	for i, m := range raw {
		out[i] = SDKModel{
			ID: m.ID, Name: m.Name, Vision: m.Capabilities.Supports.Vision,
			ReasoningEffort: m.Capabilities.Supports.ReasoningEffort,
			Efforts:         m.SupportedReasoningEfforts, DefaultEffort: m.DefaultReasoningEffort,
			Enabled: m.Policy == nil || m.Policy.State == "enabled",
		}
		if m.Billing != nil {
			out[i].Multiplier = m.Billing.Multiplier
		}
		if lim := m.Capabilities.Limits.MaxContextWindowTokens; lim != nil {
			out[i].ContextTokens = *lim
		}
	}
	return out, nil
}

// newOrResumedSession resumes o.SessionID when set, else creates a fresh session.
func newOrResumedSession(ctx context.Context, client *copilot.Client, o sdkOptions, approved *atomic.Bool) (*copilot.Session, error) {
	if o.multiProvider() {
		if err := probeMultiProvider(ctx, client); err != nil {
			return nil, err
		}
	}
	var session *copilot.Session
	var err error
	if o.SessionID == "" {
		cfg := buildSessionConfig(o)
		cfg.OnPermissionRequest = permissionHandler(o, approved)
		if session, err = client.CreateSession(ctx, cfg); err != nil {
			return nil, fmt.Errorf("create session: %w", err)
		}
	} else {
		cfg := buildResumeSessionConfig(o)
		cfg.OnPermissionRequest = permissionHandler(o, approved)
		if session, err = client.ResumeSessionWithOptions(ctx, o.SessionID, cfg); err != nil {
			return nil, fmt.Errorf("resume session %s: %w", o.SessionID, err)
		}
	}
	// a resumed session may carry another wire's saved model: re-pin before any turn
	if o.multiProvider() {
		if err := selectRegistered(ctx, session, o); err != nil {
			return nil, errors.Join(err, session.Disconnect())
		}
	}
	return session, nil
}

// startClient starts the runtime; the returned stop joins any stop error into *errp.
func startClient(ctx context.Context, o sdkOptions, errp *error) (*copilot.Client, func(), error) {
	clientOpts, err := buildClientOptions(ctx, o)
	if err != nil {
		return nil, nil, err
	}
	client, err := startRuntime(ctx, clientOpts)
	if err != nil {
		return nil, nil, fmt.Errorf("start copilot runtime: %w", err)
	}
	// SDK starts the CLI outside childCmd: ForceStop on ctx cancel kills it (direct child only, no process group)
	stop := context.AfterFunc(ctx, client.ForceStop)
	return client, func() {
		// before stop(): a cancel during Stop still force-stops
		if stopErr := client.Stop(); stopErr != nil {
			*errp = errors.Join(*errp, fmt.Errorf("stop copilot runtime: %w", stopErr))
		}
		stop()
	}, nil
}

// startRuntimeFn is the seam tests stub to fake a runtime handshake.
var startRuntimeFn = func(ctx context.Context, opts *copilot.ClientOptions) (*copilot.Client, error) {
	client := copilot.NewClient(opts)
	return client, client.Start(ctx)
}

// lastProtocol3CLI is the newest Copilot CLI release whose runtime speaks SDK protocol 3; bump it with the SDK.
const lastProtocol3CLI = "1.0.112"

// startRuntime retries a protocol mismatch once on the newest cached runtime the SDK speaks (the CLI self-updates).
func startRuntime(ctx context.Context, opts *copilot.ClientOptions) (*copilot.Client, error) {
	client, err := startRuntimeFn(ctx, opts)
	if err == nil || !strings.Contains(err.Error(), "SDK protocol version mismatch") {
		return client, err
	}
	conn, ok := opts.Connection.(copilot.StdioConnection)
	if !ok {
		return nil, err
	}
	version := cachedRuntime(conn.Env)
	if version == "" {
		return nil, fmt.Errorf("%w; Copilot CLI %s at %s needs SDK protocol %d and none is cached; fix: "+
			"curl -fsSL https://gh.io/copilot-install | VERSION=%s PREFIX=%s bash", err, cliVersion(ctx, conn.Path),
			conn.Path, copilot.SDKProtocolVersion, lastProtocol3CLI, filepath.Dir(filepath.Dir(conn.Path)))
	}
	fmt.Fprintf(os.Stderr, "copilot: installed runtime speaks another SDK protocol; using cached %s (protocol %d)\n",
		version, copilot.SDKProtocolVersion)
	conn.Args = append(slices.Clone(conn.Args), "--prefer-version", version)
	retry := *opts
	retry.Connection = conn
	return startRuntimeFn(ctx, &retry)
}

// cachedRuntime is the newest runtime in the CLI's package cache (its own search roots) on the SDK's protocol.
func cachedRuntime(env []string) string {
	best := ""
	for _, root := range runtimeCacheRoots(env) {
		files, _ := filepath.Glob(filepath.Join(root, "pkg", "*", "*", "copilot-sdk", "sdkProtocolVersion.d.ts"))
		for _, f := range files {
			v := compatibleRuntime(f)
			if c, ok := textutil.CompareVersions(v, best); v != "" && (best == "" || ok && c > 0) {
				best = v
			}
		}
	}
	return best
}

// runtimeCacheRoots mirrors the CLI loader's search roots; relative ones (an unset name) are dropped.
func runtimeCacheRoots(env []string) []string {
	get := func(k string) string {
		v := ""
		for _, kv := range env {
			if name, val, ok := strings.Cut(kv, "="); ok && name == k {
				v = val
			}
		}
		return v
	}
	home, _ := os.UserHomeDir()
	cache, _ := os.UserCacheDir()
	roots := []string{get("COPILOT_PKG_CACHE_HOME"), get("COPILOT_CACHE_HOME"), filepath.Join(cache, "copilot"),
		get("COPILOT_HOME"), filepath.Join(home, ".copilot")}
	return slices.DeleteFunc(roots, func(r string) bool { return !filepath.IsAbs(r) })
}

// compatibleRuntime is the version dir holding f when it is a complete runtime on the SDK's protocol, else "".
func compatibleRuntime(f string) string {
	dir := filepath.Dir(filepath.Dir(f))
	if _, err := os.Stat(filepath.Join(dir, "index.js")); err != nil {
		return ""
	}
	b, err := os.ReadFile(f)
	if err != nil {
		return ""
	}
	_, rest, _ := strings.Cut(string(b), "SDK_PROTOCOL_VERSION = ")
	num, _, _ := strings.Cut(rest, ";")
	if n, err := strconv.Atoi(strings.TrimSpace(num)); err != nil || n != copilot.SDKProtocolVersion {
		return ""
	}
	return filepath.Base(dir)
}

// cliVersion is the installed CLI's own version, read within a second; "unknown" otherwise.
func cliVersion(ctx context.Context, path string) string {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	fields := strings.Fields(strings.SplitN(string(out), "\n", 2)[0])
	if err != nil || len(fields) == 0 {
		return "unknown"
	}
	return strings.TrimSuffix(fields[len(fields)-1], ".")
}

// sdkAsk runs one prompt to completion; assistant events arrive on the SDK goroutine, hence the mutex.
func sdkAsk(ctx context.Context, o sdkOptions, prompt string, images []string) (result sdkResult, err error) {
	if strings.TrimSpace(prompt) == "" {
		return sdkResult{}, errors.New("empty prompt")
	}
	client, stopClient, err := startClient(ctx, o, &err)
	if err != nil {
		return sdkResult{}, err
	}
	defer stopClient()

	var approved atomic.Bool
	session, err := newOrResumedSession(ctx, client, o, &approved)
	if err != nil {
		return sdkResult{}, err
	}
	defer func() {
		if disconnectErr := session.Disconnect(); disconnectErr != nil {
			err = errors.Join(err, fmt.Errorf("disconnect copilot session: %w", disconnectErr))
		}
	}()

	col := assistantCollector{onLimit: func(id string) { // cancel, or the headless turn waits on a user who is not there
		cancel := rpc.UISessionLimitsExhaustedResponse{Action: rpc.UISessionLimitsExhaustedResponseActionCancel}
		_, _ = session.RPC.UI.HandlePendingSessionLimitsExhausted(context.WithoutCancel(ctx),
			&rpc.UIHandlePendingSessionLimitsExhaustedRequest{RequestID: id, Response: cancel})
	}}
	unsubscribe := session.On(col.onEvent)
	defer unsubscribe()

	sendErr := sendPrompt(ctx, session, o, prompt, images)
	text, model, usage := col.snapshot()
	res := sdkResult{Text: text, SessionID: session.SessionID, Model: model, ToolRan: approved.Load(), Usage: usage}
	res.FinishReason, res.ContentFiltered = col.finish()
	// the partial text and session id still ride back, so a caller can hand the task off
	if sendErr != nil {
		return res, fmt.Errorf("send prompt: %w", sendErr)
	}
	return res, errors.Join(col.limitError(), truncationError(res))
}

// assistantCollector accumulates assistant text, model and usage; events arrive on the SDK goroutine, hence the mutex.
type assistantCollector struct {
	mu   sync.Mutex
	text strings.Builder
	// the task_complete summary: some models answer only there, with empty message content
	summary string
	model   string
	usage   wire.Usage
	finishR string
	filter  bool
	limit   *copilot.SessionLimitsExhaustedRequestedData
	onLimit func(requestID string)
}

// limitError names the budgetCredits cap when the SDK reported it reached; nil otherwise.
func (c *assistantCollector) limitError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.limit == nil {
		return nil
	}
	return fmt.Errorf("copilot budgetCredits cap reached (%.4g of %.4g AI credits used)",
		c.limit.UsedAiCredits, c.limit.MaxAiCredits)
}

func (c *assistantCollector) onEvent(ev copilot.SessionEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	root := ev.AgentID == nil || *ev.AgentID == "" // sub-agent text and model are not the answer; their usage is still billed
	switch d := ev.Data.(type) {
	case *copilot.AssistantMessageData:
		if root {
			c.addMessage(d)
		}
	case *copilot.AssistantUsageData:
		c.addUsage(d)
	case *copilot.SessionLimitsExhaustedRequestedData:
		c.limit = d
		if c.onLimit != nil {
			go c.onLimit(d.RequestID)
		}
	case *copilot.SessionTaskCompleteData:
		if root && d.Summary != nil {
			c.summary = *d.Summary
		}
	}
}

// addMessage appends a whole assistant message (not a delta), keeping turns apart.
func (c *assistantCollector) addMessage(d *copilot.AssistantMessageData) {
	if strings.TrimSpace(d.Content) != "" {
		if c.text.Len() > 0 {
			c.text.WriteString("\n\n")
		}
		c.text.WriteString(d.Content)
	}
	if d.Model != nil {
		c.model = *d.Model
	}
}

// addUsage sums one call's usage; the SDK's inputTokens include cache reads and writes, wire.Usage keeps them apart.
func (c *assistantCollector) addUsage(d *copilot.AssistantUsageData) {
	in, out := tokenCount(d.InputTokens), tokenCount(d.OutputTokens)
	read, write := tokenCount(d.CacheReadTokens), tokenCount(d.CacheWriteTokens)
	c.usage.InputTokens += max(in-read-write, 0)
	c.usage.CacheReadTokens += read
	c.usage.CacheCreationTokens += write
	c.usage.OutputTokens += out
	if d.FinishReason != nil {
		c.finishR = *d.FinishReason
	}
	c.filter = c.filter || (d.ContentFilterTriggered != nil && *d.ContentFilterTriggered)
}

// finish returns the last call's finish reason and whether any call tripped the content filter.
func (c *assistantCollector) finish() (reason string, filtered bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.finishR, c.filter
}

// truncationError is non-nil when the answer was cut off by the token limit or a content filter.
func truncationError(r sdkResult) error {
	switch {
	case r.ContentFiltered || r.FinishReason == "content_filter":
		return errors.New("copilot answer blocked or truncated by the content filter")
	case r.FinishReason == "length":
		return errors.New("copilot answer cut off at the output token limit (finish_reason=length)")
	}
	return nil
}

func tokenCount(p *int64) int {
	if p == nil {
		return 0
	}
	return int(*p)
}

// snapshot returns the text plus any task_complete summary it lacks, the last model and the summed usage.
func (c *assistantCollector) snapshot() (text, model string, usage wire.Usage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	text, summary := strings.TrimSpace(c.text.String()), strings.TrimSpace(c.summary)
	// the answer may sit only in task_complete, after a preamble
	if summary != "" && !strings.Contains(text, summary) {
		text = strings.TrimSpace(text + "\n\n" + summary)
	}
	return text, c.model, c.usage
}

// sendPrompt sends the prompt (fleet mode when asked) and waits for the turn to finish.
func sendPrompt(ctx context.Context, session *copilot.Session, o sdkOptions, prompt string, images []string) error {
	msgOpts := copilot.MessageOptions{Prompt: prompt, Attachments: fileAttachments(images)}
	if o.Autonomous {
		msgOpts.AgentMode = copilot.AgentModeAutopilot
	}
	if o.Fleet {
		return startFleet(ctx, session.RPC.Fleet, msgOpts)
	}
	_, err := session.SendAndWait(ctx, msgOpts)
	return err
}

// fileAttachments wraps absolute paths as SDK file attachments (the runtime reads and sends images natively).
func fileAttachments(paths []string) []copilot.Attachment {
	var out []copilot.Attachment
	for _, p := range paths {
		out = append(out, &copilot.AttachmentFile{Path: p, DisplayName: filepath.Base(p)})
	}
	return out
}

// fleetStarter is the slice of the SDK's fleet API startFleet needs.
type fleetStarter interface {
	Start(ctx context.Context, params *rpc.FleetStartRequest) (*rpc.FleetStartResult, error)
}

// startFleet runs the prompt in fleet mode and waits for it to finish.
func startFleet(ctx context.Context, f fleetStarter, m copilot.MessageOptions) error {
	wait := true
	res, err := f.Start(ctx, &rpc.FleetStartRequest{Prompt: &m.Prompt, Wait: &wait, Attachments: m.Attachments})
	if err != nil {
		return err
	}
	if res == nil || !res.Started {
		return errors.New("copilot: fleet mode did not start")
	}
	return nil
}

// minMultiProviderCLI is the oldest copilot CLI proven to serve SDK named providers (probed 2026-10-28).
const minMultiProviderCLI = "1.0.104"

// Named failures of the multi-provider route; callers match with errors.Is.
var (
	errNoMultiProvider  = errors.New("copilot CLI lacks named-provider support")
	errUnregisteredPick = errors.New("model is not in the session registry")
)

// registryEnv is base minus the exact unset names and any stripPrefix name, plus set.
func registryEnv(base, set, unset []string, stripPrefix string) []string {
	out := make([]string, 0, len(base)+len(set))
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		if (stripPrefix != "" && strings.HasPrefix(name, stripPrefix)) || slices.Contains(unset, name) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, set...)
}

// validateRegistryOptions fails closed before any runtime starts: no singular BYOK, a token source, a registered pick.
func validateRegistryOptions(o sdkOptions) error {
	switch {
	case o.Token == nil:
		return errors.New("copilot registry route needs a session token source")
	case len(o.ChildEnv) == 0:
		return errors.New("copilot registry route needs the private home env (ChildEnv)")
	case o.GitHubToken != "" || o.UseLoggedInUser:
		return errors.New("copilot registry route must not carry a GitHub login")
	}
	for _, kv := range o.Env {
		if strings.HasPrefix(kv, proc.CopilotProviderPrefix) || strings.HasPrefix(kv, proc.EnvCopilotProvidersConfig+"=") {
			return errors.New("copilot registry route cannot mix with a singular provider env block")
		}
	}
	for _, m := range o.Models {
		if m.Ref() == o.Model {
			return nil
		}
	}
	return fmt.Errorf("%w: %q", errUnregisteredPick, o.Model)
}

func (o sdkOptions) multiProvider() bool { return len(o.Providers) > 0 || len(o.Models) > 0 }

// namedProviders builds the SDK providers with credentials cleared; every one's auth goes through the token callback.
func namedProviders(o sdkOptions) ([]copilot.NamedProviderConfig, []copilot.ProviderModelConfig) {
	cb := func(copilot.ProviderTokenArgs) (string, error) { return o.Token() }
	ps := make([]copilot.NamedProviderConfig, len(o.Providers))
	for i, p := range o.Providers {
		ps[i] = copilot.NamedProviderConfig{Name: p.Name, Type: p.Type, WireAPI: p.WireAPI, BaseURL: p.BaseURL,
			Headers: p.Headers, APIKey: "", BearerToken: "", BearerTokenProvider: cb}
	}
	ms := make([]copilot.ProviderModelConfig, len(o.Models))
	for i, m := range o.Models {
		ms[i] = copilot.ProviderModelConfig{ID: m.ID, Provider: m.Provider, WireModel: m.WireModel, ModelID: m.ModelID,
			Name: m.Name, MaxPromptTokens: m.MaxPromptTokens, MaxContextWindowTokens: m.MaxContextWindowTokens,
			MaxOutputTokens: m.MaxOutputTokens}
	}
	return ps, ms
}

// probeMultiProvider gates the route on the running CLI's version: older runtimes ignore named providers.
func probeMultiProvider(ctx context.Context, client *copilot.Client) error {
	st, err := client.GetStatus(ctx)
	if err != nil {
		return fmt.Errorf("%w: cannot read CLI version: %v", errNoMultiProvider, err)
	}
	if order, ok := textutil.CompareVersions(st.Version, minMultiProviderCLI); !ok || order < 0 {
		return fmt.Errorf("%w: CLI %q is older than %s (run `copilot update`)",
			errNoMultiProvider, st.Version, minMultiProviderCLI)
	}
	return nil
}

// selectRegistered pins the session to o.Model; an unknown id errors, so a pick never falls through to GitHub.
func selectRegistered(ctx context.Context, session *copilot.Session, o sdkOptions) error {
	var opts *copilot.SetModelOptions
	if o.ReasoningEffort != "" {
		opts = &copilot.SetModelOptions{ReasoningEffort: &o.ReasoningEffort}
	}
	err := session.SetModel(ctx, o.Model, opts)
	var rpcErr *copilot.RPCError
	switch {
	case err == nil:
		return nil
	// the runtime answered and refused the id; transport and ctx failures keep their own error
	case errors.As(err, &rpcErr):
		return fmt.Errorf("%w: select %s: %w", errUnregisteredPick, o.Model, err)
	default:
		return fmt.Errorf("select %s: %w", o.Model, err)
	}
}
