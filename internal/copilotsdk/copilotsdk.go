// Package copilotsdk boxes github.com/github/copilot-sdk/go: one headless Copilot turn over the real CLI.
package copilotsdk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"

	"github.com/edotau/claude-code/internal/envscrub"
)

// DefaultClientName identifies this host in the SDK's User-Agent.
const DefaultClientName = "claude-code"

// denyFeedback tells the model to re-plan in prose instead of retrying a refused tool call.
const denyFeedback = "Tool use is disabled for this headless run. Answer from the prompt alone."

// Options configures one headless run. Env carries the COPILOT_PROVIDER_* BYOK block from vendors.Configure.
type Options struct {
	CLIPath          string   // resolved copilot binary; empty is an error (never the SDK's bundled runtime)
	Env              []string // KEY=VALUE BYOK block
	KeepEnv          []string // names the credential scrub must not drop, from --keep-env
	Model            string   // Copilot-catalog id; leave empty on BYOK (COPILOT_MODEL carries it)
	ReasoningEffort  string
	Context          string // ContextTier: "default" or "long_context"; empty leaves the model's own default
	WorkingDirectory string
	AllowAllTools    bool
	UseLoggedInUser  bool // always explicit: unset, the SDK silently falls back to the GitHub subscription
	GitHubToken      string
	Autonomous       bool   // MessageOptions.AgentMode = AgentModeAutopilot
	EnableMemory     bool   // SessionConfig.Memory.Enabled
	SessionID        string // non-empty resumes this session instead of creating a new one
}

// Result is one completed turn; Model is what the runtime reports actually served it.
type Result struct {
	Text      string
	SessionID string
	Model     string
	Duration  time.Duration
}

// BuildClientOptions pins stdio at an explicit CLI path; Env on the connection is the child's complete env.
func BuildClientOptions(o Options) (*copilot.ClientOptions, error) {
	if strings.TrimSpace(o.CLIPath) == "" {
		return nil, errors.New("copilot CLI path is empty (the SDK would run its bundled runtime instead)")
	}
	base, _ := envscrub.ScrubVendor(os.Environ(), envscrub.KeepSet(o.KeepEnv))
	return &copilot.ClientOptions{
		Connection:       copilot.StdioConnection{Path: o.CLIPath, Env: append(base, o.Env...)},
		WorkingDirectory: o.WorkingDirectory,
		UseLoggedInUser:  new(o.UseLoggedInUser),
		GitHubToken:      o.GitHubToken,
	}, nil
}

// BuildSessionConfig denies tool calls unless AllowAllTools is set.
func BuildSessionConfig(o Options) *copilot.SessionConfig {
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
	return cfg
}

// buildResumeSessionConfig mirrors BuildSessionConfig for client.ResumeSessionWithOptions.
func buildResumeSessionConfig(o Options) *copilot.ResumeSessionConfig {
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
	return cfg
}

func permissionHandler(o Options) copilot.PermissionHandlerFunc {
	if o.AllowAllTools {
		return approveToolOnce
	}
	return denyAllTools
}

// providerFromEnv lifts the BYOK block into the session: the server-mode runtime ignores COPILOT_PROVIDER_* env.
func providerFromEnv(env []string) *copilot.ProviderConfig {
	get := func(key string) string {
		for _, kv := range env {
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
	return &copilot.ProviderConfig{
		Type:        get("COPILOT_PROVIDER_TYPE"),
		BaseURL:     base,
		APIKey:      get("COPILOT_PROVIDER_API_KEY"),
		BearerToken: get("COPILOT_PROVIDER_BEARER_TOKEN"),
		ModelID:     get("COPILOT_MODEL"),
		WireModel:   get("COPILOT_MODEL"),
	}
}

func denyAllTools(_ copilot.PermissionRequest, _ copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
	feedback := denyFeedback
	return &rpc.PermissionDecisionReject{Feedback: &feedback}, nil
}

// approveToolOnce is the explicit opt-in; managed org settings still win.
func approveToolOnce(_ copilot.PermissionRequest, inv copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
	if inv.ManagedSettingsEnabled {
		return &rpc.PermissionDecisionNoResult{}, nil
	}
	return &rpc.PermissionDecisionApproveOnce{}, nil
}

// Model is the catalog entry the harness prints and validates pins against.
type Model struct {
	ID              string
	Name            string
	Vision          bool
	ReasoningEffort bool
	Multiplier      *float64 // nil when the model carries no premium billing multiplier
}

// listModelsFn is the injectable seam: tests fake the copilot CLI's catalog without a real GitHub login.
var listModelsFn = func(ctx context.Context, o Options) (models []copilot.ModelInfo, err error) {
	clientOpts, err := BuildClientOptions(o)
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

// ListModels returns the copilot CLI's own model catalog via its logged-in GitHub session (never BYOK).
func ListModels(ctx context.Context, o Options) ([]Model, error) {
	raw, err := listModelsFn(ctx, o)
	if err != nil {
		return nil, fmt.Errorf("list copilot models (sign in with `copilot` first, then retry): %w", err)
	}
	out := make([]Model, len(raw))
	for i, m := range raw {
		out[i] = Model{ID: m.ID, Name: m.Name, Vision: m.Capabilities.Supports.Vision, ReasoningEffort: m.Capabilities.Supports.ReasoningEffort}
		if m.Billing != nil {
			out[i].Multiplier = m.Billing.Multiplier
		}
	}
	return out, nil
}

// newOrResumedSession resumes o.SessionID when set, else creates a fresh session.
func newOrResumedSession(ctx context.Context, client *copilot.Client, o Options) (*copilot.Session, error) {
	if o.SessionID == "" {
		session, err := client.CreateSession(ctx, BuildSessionConfig(o))
		if err != nil {
			return nil, fmt.Errorf("create session: %w", err)
		}
		return session, nil
	}
	session, err := client.ResumeSessionWithOptions(ctx, o.SessionID, buildResumeSessionConfig(o))
	if err != nil {
		return nil, fmt.Errorf("resume session %s: %w", o.SessionID, err)
	}
	return session, nil
}

// Ask runs one prompt to completion; assistant events arrive on the SDK goroutine, hence the mutex.
func Ask(ctx context.Context, o Options, prompt string) (result Result, err error) {
	if strings.TrimSpace(prompt) == "" {
		return Result{}, errors.New("empty prompt")
	}
	clientOpts, err := BuildClientOptions(o)
	if err != nil {
		return Result{}, err
	}
	client := copilot.NewClient(clientOpts)
	if err := client.Start(ctx); err != nil {
		return Result{}, fmt.Errorf("start copilot runtime: %w", err)
	}
	defer func() {
		if stopErr := client.Stop(); stopErr != nil {
			err = errors.Join(err, fmt.Errorf("stop copilot runtime: %w", stopErr))
		}
	}()

	session, err := newOrResumedSession(ctx, client, o)
	if err != nil {
		return Result{}, err
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

	msgOpts := copilot.MessageOptions{Prompt: prompt}
	if o.Autonomous {
		msgOpts.AgentMode = copilot.AgentModeAutopilot
	}
	start := time.Now()
	if _, err := session.SendAndWait(ctx, msgOpts); err != nil {
		return Result{}, fmt.Errorf("send prompt: %w", err)
	}
	mu.Lock()
	defer mu.Unlock()
	return Result{Text: strings.TrimSpace(text.String()), SessionID: session.SessionID, Model: model, Duration: time.Since(start)}, nil
}
