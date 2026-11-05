// The SDK box: one headless Copilot turn over the real CLI. In sdk*.go, `copilot.` is the SDK import.

package copilot

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/shell"
	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

// DefaultClientName identifies this host in the SDK's User-Agent.
const DefaultClientName = "claude-code"

// denyFeedback tells the model to re-plan in prose instead of retrying a refused tool call.
const denyFeedback = "Tool use is disabled for this headless run. Answer from the prompt alone."

// SDKOptions configures one headless run. Env carries the COPILOT_PROVIDER_* BYOK block from agent.Configure.
type SDKOptions struct {
	CLIPath          string   // resolved copilot binary; empty is an error (never the SDK's bundled runtime)
	Env              []string // KEY=VALUE BYOK block
	KeepEnv          []string // names the credential scrub must not drop, from --keep-env
	Model            string   // Copilot-catalog id; leave empty on BYOK (COPILOT_MODEL carries it)
	ReasoningEffort  string
	Context          string // ContextTier: "default" or "long_context"; empty leaves the model's own default
	WorkingDirectory string
	AllowAllTools    bool
	ReadOnlyTools    bool // with AllowAllTools: approve only file reads and URL fetches
	UseLoggedInUser  bool // always explicit: unset, the SDK silently falls back to the GitHub subscription
	GitHubToken      string
	Autonomous       bool   // MessageOptions.AgentMode = AgentModeAutopilot
	Fleet            bool   // experimental session.RPC.Fleet.Start instead of SendAndWait
	EnableMemory     bool   // SessionConfig.Memory.Enabled
	SessionID        string // non-empty resumes this session instead of creating a new one
	// Providers+Models switch to the multi-provider route: Model is a provider/id of Models, Token feeds every
	// provider's BearerTokenProvider (set by the SDK layer), and the child runs in a private login-free home.
	Providers []NamedProvider
	Models    []RegistryModel
	Token     func() (string, error)
	// ChildEnv is set on, and ChildUnset (exact names) plus ChildUnsetPrefix stripped from, the registry child's env.
	ChildEnv, ChildUnset []string
	ChildUnsetPrefix     string
}

// SDKResult is one completed turn; Model is what the runtime reports actually served it.
type SDKResult struct {
	Text      string
	SessionID string
	Model     string
	Duration  time.Duration
	ToolRan   bool // a tool request was approved, so the run may have side effects
	Usage     agent.Usage
}

// BuildClientOptions pins stdio at an explicit CLI path; Env on the connection is the child's complete env.
func BuildClientOptions(ctx context.Context, o SDKOptions) (*copilot.ClientOptions, error) {
	if strings.TrimSpace(o.CLIPath) == "" {
		return nil, errors.New("copilot CLI path is empty (the SDK would run its bundled runtime instead)")
	}
	base := agent.EnvSpec{Mode: agent.EnvAgent, Keep: o.KeepEnv}.Environ(ctx) // isolation and unset ride ctx
	if o.multiProvider() {
		if err := validateRegistryOptions(o); err != nil {
			return nil, err
		}
		base = registryEnv(append(base, o.Env...), o.ChildEnv, o.ChildUnset, o.ChildUnsetPrefix)
		o.Env = nil
	}
	env := append(base, o.Env...)
	if o.ReadOnlyTools { // applied last so neither the parent's env nor --env can re-arm a git helper
		env = registryEnv(env, readOnlyGitEnv, nil, "GIT_")
	}
	recorded := env
	if o.GitHubToken != "" { // the SDK sets it on the child itself (copilot-sdk client.go:2282)
		recorded = append(slices.Clone(env), "COPILOT_SDK_AUTH_TOKEN="+o.GitHubToken)
	}
	agent.RecordOverlayDebug("copilot", recorded)
	return &copilot.ClientOptions{
		Connection:       copilot.StdioConnection{Path: o.CLIPath, Env: env},
		WorkingDirectory: o.WorkingDirectory,
		UseLoggedInUser:  new(o.UseLoggedInUser),
		GitHubToken:      o.GitHubToken,
	}, nil
}

// BuildSessionConfig denies tool calls unless AllowAllTools is set.
func BuildSessionConfig(o SDKOptions) *copilot.SessionConfig {
	cfg := &copilot.SessionConfig{
		ClientName:          DefaultClientName,
		Model:               o.Model,
		ReasoningEffort:     o.ReasoningEffort,
		ContextTier:         copilot.ContextTier(o.Context),
		WorkingDirectory:    o.WorkingDirectory,
		Provider:            providerFromEnv(o.Env),
		OnPermissionRequest: permissionHandler(o, nil),
	}
	if o.EnableMemory {
		cfg.Memory = &copilot.MemoryConfiguration{Enabled: true}
	}
	if o.multiProvider() {
		cfg.Provider = nil
		cfg.Providers, cfg.Models = namedProviders(o)
	}
	return cfg
}

// buildResumeSessionConfig mirrors BuildSessionConfig for client.ResumeSessionWithOptions.
func buildResumeSessionConfig(o SDKOptions) *copilot.ResumeSessionConfig {
	cfg := &copilot.ResumeSessionConfig{
		ClientName:          DefaultClientName,
		Model:               o.Model,
		ReasoningEffort:     o.ReasoningEffort,
		ContextTier:         copilot.ContextTier(o.Context),
		WorkingDirectory:    o.WorkingDirectory,
		Provider:            providerFromEnv(o.Env),
		OnPermissionRequest: permissionHandler(o, nil),
	}
	if o.EnableMemory {
		cfg.Memory = &copilot.MemoryConfiguration{Enabled: true}
	}
	if o.multiProvider() {
		cfg.Provider = nil
		cfg.Providers, cfg.Models = namedProviders(o)
	}
	return cfg
}

// permissionHandler wraps baseHandler to record an approval in approved (non-nil; handlers run on SDK goroutines).
func permissionHandler(o SDKOptions, approved *atomic.Bool) copilot.PermissionHandlerFunc {
	h := baseHandler(o)
	if approved == nil {
		return h
	}
	return func(req copilot.PermissionRequest, inv copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
		d, err := h(req, inv)
		if _, ok := d.(*rpc.PermissionDecisionApproveOnce); ok && err == nil {
			approved.Store(true)
		}
		return d, err
	}
}

func baseHandler(o SDKOptions) copilot.PermissionHandlerFunc {
	if o.AllowAllTools && o.ReadOnlyTools {
		return approveReadOnly
	}
	if o.AllowAllTools && o.Autonomous {
		return approveToolOnce
	}
	if o.AllowAllTools {
		return approveScoped(o.WorkingDirectory)
	}
	return denyAllTools
}

// providerFromEnv lifts the BYOK block into the session: the server-mode runtime ignores COPILOT_PROVIDER_* env.
// Last duplicate wins (exec's rule): agent.copilotWire blanks each credential var, then appends the real one.
func providerFromEnv(env []string) *copilot.ProviderConfig {
	get := func(key string) string {
		for _, kv := range slices.Backward(env) {
			if v, ok := strings.CutPrefix(kv, key+"="); ok {
				return v
			}
		}
		return ""
	}
	base := get("COPILOT_PROVIDER_BASE_URL")
	if base == "" {
		return nil
	}
	atoi := func(key string) int { n, _ := strconv.Atoi(get(key)); return n }
	model := get("COPILOT_MODEL")
	modelID := WellKnownModel(model)
	if id := get("COPILOT_PROVIDER_MODEL_ID"); id != "" {
		modelID = id
	}
	return &copilot.ProviderConfig{
		Type:            get("COPILOT_PROVIDER_TYPE"),
		WireAPI:         get("COPILOT_PROVIDER_WIRE_API"),
		BaseURL:         base,
		APIKey:          get("COPILOT_PROVIDER_API_KEY"),
		BearerToken:     get("COPILOT_PROVIDER_BEARER_TOKEN"),
		ModelID:         modelID,
		WireModel:       model,
		MaxPromptTokens: atoi("COPILOT_PROVIDER_MAX_PROMPT_TOKENS"),
		MaxOutputTokens: atoi("COPILOT_PROVIDER_MAX_OUTPUT_TOKENS"),
	}
}

// WellKnownModel maps a gateway Claude id to copilot's catalog form (claude-sonnet-5.5), so the runtime applies
// its Claude settings (no temperature, reasoning, limits); any other id passes through unchanged.
func WellKnownModel(id string) string {
	canonical, _, ok := models.Resolve(id)
	if !ok || models.FamilyKind(canonical) != models.FamilyClaude {
		return id
	}
	return models.Wire("dotted", canonical)
}

func denyAllTools(_ copilot.PermissionRequest, _ copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
	feedback := denyFeedback
	return &rpc.PermissionDecisionReject{Feedback: &feedback}, nil
}

// readOnlyKinds are the requests a read-only run approves: file reads and URL fetches, never write or memory.
var readOnlyKinds = map[rpc.PermissionRequestKind]bool{rpc.PermissionRequestKindRead: true, rpc.PermissionRequestKindURL: true}

const readOnlyFeedback = "This run is read-only: only file reads, URL fetches and read-only git (status, diff, log, show, blame, rev-parse, branch --list) are allowed. git diff, log and show must start with --no-ext-diff --no-textconv, git blame with --no-textconv (git log --no-ext-diff --no-textconv -p). Report instead of changing anything."

// readOnlyGitEnv replaces all inherited GIT_*; GIT_PAGER beats pager.<verb>, command-scope config beats repo files.
var readOnlyGitEnv = slices.Concat([]string{
	"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat",
	"GIT_NO_LAZY_FETCH=1", "GIT_ALLOW_PROTOCOL=none", // a partial clone's missing blob must not reach a transport
	"GIT_OPTIONAL_LOCKS=0", // status and diff must not rewrite the index
}, gitConfigEnv(
	"core.fsmonitor=false", "core.pager=cat", "core.sshCommand=false", "core.hooksPath=/dev/null",
	"diff.external=", "diff.submodule=short", "log.showSignature=false", "log.diffMerges=separate",
	"gpg.program=false", "gpg.openpgp.program=false", "gpg.x509.program=false", "gpg.ssh.program=false",
))

// gitConfigEnv spells key=value pairs as the GIT_CONFIG_COUNT block.
func gitConfigEnv(pairs ...string) []string {
	out := []string{"GIT_CONFIG_COUNT=" + strconv.Itoa(len(pairs))}
	for i, kv := range pairs {
		k, v, _ := strings.Cut(kv, "=")
		out = append(out, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, k), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, v))
	}
	return out
}

func shellCommand(req copilot.PermissionRequest) (string, bool) {
	switch r := req.(type) {
	case rpc.PermissionRequestShell:
		return r.FullCommandText, !r.HasWriteFileRedirection
	case *rpc.PermissionRequestShell:
		return r.FullCommandText, !r.HasWriteFileRedirection
	}
	return "", false
}

func approveReadOnly(req copilot.PermissionRequest, inv copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
	cmd, ok := shellCommand(req)
	if !readOnlyKinds[req.Kind()] && (!ok || !shell.ReadOnlyGit(cmd)) {
		feedback := readOnlyFeedback
		return &rpc.PermissionDecisionReject{Feedback: &feedback}, nil
	}
	return approveToolOnce(req, inv)
}

// approveToolOnce is the explicit opt-in; managed org settings still win.
func approveToolOnce(_ copilot.PermissionRequest, inv copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
	if inv.ManagedSettingsEnabled {
		return &rpc.PermissionDecisionNoResult{}, nil
	}
	return &rpc.PermissionDecisionApproveOnce{}, nil
}

// SDKModel is the catalog entry the harness prints and validates pins against.
type SDKModel struct {
	ID              string
	Name            string
	Vision          bool
	ReasoningEffort bool
	Multiplier      *float64 // nil when the model carries no premium billing multiplier
	Efforts         []string // SupportedReasoningEfforts; empty when the model has none
	DefaultEffort   string   // "" when the catalog names none
	ContextTokens   int      // MaxContextWindowTokens; 0 when the catalog reports none
	Enabled         bool     // Policy == nil or Policy.State == "enabled"
}

// CatalogOptions is the auth shape every catalog listing needs: a resolved launch-account token
// (CopilotToken), or "" to fall back to the copilot CLI's own stored login.
func CatalogOptions(cliPath, token string) SDKOptions {
	if token != "" {
		return SDKOptions{CLIPath: cliPath, GitHubToken: token}
	}
	return SDKOptions{CLIPath: cliPath, UseLoggedInUser: true}
}

// listModelsFn is the injectable seam: tests fake the copilot CLI's catalog without a real GitHub login.
var listModelsFn = func(ctx context.Context, o SDKOptions) (models []copilot.ModelInfo, err error) {
	clientOpts, err := BuildClientOptions(ctx, o)
	if err != nil {
		return nil, err
	}
	client := copilot.NewClient(clientOpts)
	if err := client.Start(ctx); err != nil {
		return nil, fmt.Errorf("start copilot runtime: %w", err)
	}
	defer func() {
		if stopErr := client.Stop(); stopErr != nil {
			err = errors.Join(err, fmt.Errorf("stop copilot runtime: %w", stopErr))
		}
	}()
	return client.ListModels(ctx)
}

// SDKListModels returns the copilot CLI's own model catalog via its logged-in GitHub session (never BYOK).
func SDKListModels(ctx context.Context, o SDKOptions) ([]SDKModel, error) {
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
func newOrResumedSession(ctx context.Context, client *copilot.Client, o SDKOptions, approved *atomic.Bool) (*copilot.Session, error) {
	if o.multiProvider() {
		if err := probeMultiProvider(ctx, client); err != nil {
			return nil, err
		}
	}
	var session *copilot.Session
	var err error
	if o.SessionID == "" {
		cfg := BuildSessionConfig(o)
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
	if o.multiProvider() { // a resumed session may carry another wire's saved model: re-pin before any turn
		if err := selectRegistered(ctx, session, o); err != nil {
			return nil, errors.Join(err, session.Disconnect())
		}
	}
	return session, nil
}

// SDKAsk runs one prompt to completion; assistant events arrive on the SDK goroutine, hence the mutex.
func SDKAsk(ctx context.Context, o SDKOptions, prompt string, images []string) (result SDKResult, err error) {
	if strings.TrimSpace(prompt) == "" {
		return SDKResult{}, errors.New("empty prompt")
	}
	clientOpts, err := BuildClientOptions(ctx, o)
	if err != nil {
		return SDKResult{}, err
	}
	client := copilot.NewClient(clientOpts)
	if err := client.Start(ctx); err != nil {
		return SDKResult{}, fmt.Errorf("start copilot runtime: %w", err)
	}
	// SDK starts the CLI outside childCmd: ForceStop on ctx cancel kills it (direct child only, no process group)
	stop := context.AfterFunc(ctx, client.ForceStop)
	defer stop()
	defer func() {
		if stopErr := client.Stop(); stopErr != nil {
			err = errors.Join(err, fmt.Errorf("stop copilot runtime: %w", stopErr))
		}
	}()

	var approved atomic.Bool
	session, err := newOrResumedSession(ctx, client, o, &approved)
	if err != nil {
		return SDKResult{}, err
	}
	defer func() {
		if disconnectErr := session.Disconnect(); disconnectErr != nil {
			err = errors.Join(err, fmt.Errorf("disconnect copilot session: %w", disconnectErr))
		}
	}()

	var col assistantCollector
	unsubscribe := session.On(col.onEvent)
	defer unsubscribe()

	start := time.Now()
	sendErr := sendPrompt(ctx, session, o, prompt, images)
	text, model, usage := col.snapshot()
	res := SDKResult{Text: text, SessionID: session.SessionID, Model: model, Duration: time.Since(start), ToolRan: approved.Load(), Usage: usage}
	if sendErr != nil { // the partial text and session id still ride back, so a caller can hand the task off
		return res, fmt.Errorf("send prompt: %w", sendErr)
	}
	return res, nil
}

// assistantCollector accumulates assistant text, model and usage; events arrive on the SDK goroutine, hence the mutex.
type assistantCollector struct {
	mu    sync.Mutex
	text  strings.Builder
	model string
	usage agent.Usage
}

func (c *assistantCollector) onEvent(ev copilot.SessionEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch d := ev.Data.(type) {
	case *copilot.AssistantMessageData:
		c.text.WriteString(d.Content)
		if d.Model != nil {
			c.model = *d.Model
		}
	case *copilot.AssistantUsageData:
		c.addUsage(d)
	}
}

// addUsage sums one call's usage; the SDK's inputTokens include cache reads and writes, agent.Usage keeps them apart.
func (c *assistantCollector) addUsage(d *copilot.AssistantUsageData) {
	in, out := tokenCount(d.InputTokens), tokenCount(d.OutputTokens)
	read, write := tokenCount(d.CacheReadTokens), tokenCount(d.CacheWriteTokens)
	c.usage.InputTokens += max(in-read-write, 0)
	c.usage.CacheReadTokens += read
	c.usage.CacheCreationTokens += write
	c.usage.OutputTokens += out
}

func tokenCount(p *int64) int {
	if p == nil {
		return 0
	}
	return int(*p)
}

// snapshot returns the trimmed text, last model and summed usage seen so far.
func (c *assistantCollector) snapshot() (text, model string, usage agent.Usage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.TrimSpace(c.text.String()), c.model, c.usage
}

// sendPrompt sends the prompt (fleet mode when asked) and waits for the turn to finish.
func sendPrompt(ctx context.Context, session *copilot.Session, o SDKOptions, prompt string, images []string) error {
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

// scopedFeedback is the refusal text naming what the --tools tier allows, generated from the copilot shell policy.
func scopedFeedback() string {
	return "This run is scoped: file reads, URL fetches, writes inside the workdir (not .git, .env*, state/, keys), " +
		agentconfig.ShellPolicy().Commands.Summary() +
		" are allowed. Anything else (push, reset, rm, curl, mcp, memory) is refused: report it instead."
}

// approveScoped is the --tools tier: reads, workdir-confined writes and an allowlisted shell; the rest is rejected.
func approveScoped(workDir string) copilot.PermissionHandlerFunc {
	return func(req copilot.PermissionRequest, inv copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
		if scopedAllowed(req, workDir) {
			return approveToolOnce(req, inv)
		}
		feedback := scopedFeedback()
		return &rpc.PermissionDecisionReject{Feedback: &feedback}, nil
	}
}

func scopedAllowed(req copilot.PermissionRequest, workDir string) bool {
	switch r := req.(type) {
	case rpc.PermissionRequestWrite:
		return shell.WriteAllowed(r.FileName, workDir)
	case *rpc.PermissionRequestWrite:
		return shell.WriteAllowed(r.FileName, workDir)
	}
	switch req.Kind() {
	case rpc.PermissionRequestKindRead, rpc.PermissionRequestKindURL:
		return true
	case rpc.PermissionRequestKindShell:
		cmd, ok := shellCommand(req)
		return ok && shell.Command(shell.CanonicalSelf(cmd), []string{workDir}, nil)
	}
	return false
}
