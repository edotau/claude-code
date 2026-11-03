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
	"time"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/models"
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
		OnPermissionRequest: permissionHandler(o),
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
		OnPermissionRequest: permissionHandler(o),
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

func permissionHandler(o SDKOptions) copilot.PermissionHandlerFunc {
	if o.AllowAllTools && o.ReadOnlyTools {
		return approveReadOnly
	}
	if o.AllowAllTools {
		return approveToolOnce
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

// readOnlyGit are the git subcommands a read-only run may shell out to.
var readOnlyGit = map[string]bool{"status": true, "diff": true, "log": true, "show": true, "blame": true, "rev-parse": true, "branch": true}

// gitBannedFlags also match any abbreviation git would accept (--out=x is --output=x).
var gitBannedFlags = []string{
	"--output", "--exec", "--ext-diff", "--config", "--open-files-in-pager",
	"--textconv", "--remerge-diff", "--diff-merges", "--submodule", // each still reaches a textconv or merge driver
}

// gitLeadingFlags must open the verb's args: later, a value option or -- swallows them (git diff -- --no-textconv).
var gitLeadingFlags = map[string][]string{
	"diff": {"--no-ext-diff", "--no-textconv"}, "log": {"--no-ext-diff", "--no-textconv"},
	"show": {"--no-ext-diff", "--no-textconv"}, "blame": {"--no-textconv"},
}

// gitVerbFlags are those verbs' only dash args: branch must carry --list or -l, and `git status -v` runs textconv.
var gitVerbFlags = map[string]map[string]bool{
	"branch": {"--list": true, "-l": true, "-a": true, "--all": true, "-r": true, "--remotes": true, "-v": true, "-vv": true},
	"status": {"--short": true, "-s": true, "--branch": true, "-b": true, "--long": true, "--porcelain": true, "--porcelain=v2": true,
		"-z": true, "--ignored": true, "-uno": true, "-uall": true, "--": true},
}

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

// readOnlyGitCommand reports whether cmd is a single allowlisted read-only git invocation (fail closed).
func readOnlyGitCommand(cmd string) bool {
	for _, r := range cmd {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune(" _./:=@%+,^-~", r) {
			return false
		}
	}
	f := strings.Fields(cmd)
	if len(f) < 2 || f[0] != "git" || !readOnlyGit[f[1]] {
		return false
	}
	args, lead := f[2:], gitLeadingFlags[f[1]]
	if len(args) < len(lead) || !slices.Equal(args[:len(lead)], lead) {
		return false
	}
	allowed, list := gitVerbFlags[f[1]], false
	for _, a := range args {
		if a == "-c" || strings.HasPrefix(a, "~") || bannedGitFlag(a) {
			return false
		}
		if allowed != nil && strings.HasPrefix(a, "-") && !allowed[a] {
			return false
		}
		list = list || a == "--list" || a == "-l"
	}
	return f[1] != "branch" || list
}

func bannedGitFlag(arg string) bool {
	name, _, _ := strings.Cut(arg, "=")
	if !strings.HasPrefix(name, "--") || len(name) < 3 {
		return false
	}
	for _, b := range gitBannedFlags {
		if strings.HasPrefix(b, name) {
			return true
		}
	}
	return false
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
	if !readOnlyKinds[req.Kind()] && (!ok || !readOnlyGitCommand(cmd)) {
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
func newOrResumedSession(ctx context.Context, client *copilot.Client, o SDKOptions) (*copilot.Session, error) {
	if o.multiProvider() {
		if err := probeMultiProvider(ctx, client); err != nil {
			return nil, err
		}
	}
	var session *copilot.Session
	var err error
	if o.SessionID == "" {
		if session, err = client.CreateSession(ctx, BuildSessionConfig(o)); err != nil {
			return nil, fmt.Errorf("create session: %w", err)
		}
	} else if session, err = client.ResumeSessionWithOptions(ctx, o.SessionID, buildResumeSessionConfig(o)); err != nil {
		return nil, fmt.Errorf("resume session %s: %w", o.SessionID, err)
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

	session, err := newOrResumedSession(ctx, client, o)
	if err != nil {
		return SDKResult{}, err
	}
	defer func() {
		if disconnectErr := session.Disconnect(); disconnectErr != nil {
			err = errors.Join(err, fmt.Errorf("disconnect copilot session: %w", disconnectErr))
		}
	}()

	var (
		mu    sync.Mutex
		text  strings.Builder
		model string
	)
	unsubscribe := session.On(func(ev copilot.SessionEvent) {
		d, ok := ev.Data.(*copilot.AssistantMessageData)
		if !ok {
			return
		}
		mu.Lock()
		text.WriteString(d.Content)
		if d.Model != nil {
			model = *d.Model
		}
		mu.Unlock()
	})
	defer unsubscribe()

	msgOpts := copilot.MessageOptions{Prompt: prompt, Attachments: fileAttachments(images)}
	if o.Autonomous {
		msgOpts.AgentMode = copilot.AgentModeAutopilot
	}
	start := time.Now()
	_, sendErr := session.SendAndWait(ctx, msgOpts)
	mu.Lock()
	defer mu.Unlock()
	res := SDKResult{Text: strings.TrimSpace(text.String()), SessionID: session.SessionID, Model: model, Duration: time.Since(start)}
	if sendErr != nil { // the partial text and session id still ride back, so a caller can hand the task off
		return res, fmt.Errorf("send prompt: %w", sendErr)
	}
	return res, nil
}

// fileAttachments wraps absolute paths as SDK file attachments (the runtime reads and sends images natively).
func fileAttachments(paths []string) []copilot.Attachment {
	var out []copilot.Attachment
	for _, p := range paths {
		out = append(out, &copilot.AttachmentFile{Path: p, DisplayName: filepath.Base(p)})
	}
	return out
}
