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
)

// DefaultClientName identifies this host in the SDK's User-Agent.
const DefaultClientName = "claude-code"

// denyFeedback tells the model to re-plan in prose instead of retrying a refused tool call.
const denyFeedback = "Tool use is disabled for this headless run. Answer from the prompt alone."

// Options configures one headless run. Env carries the COPILOT_PROVIDER_* BYOK block from vendors.Configure.
type Options struct {
	CLIPath          string   // resolved copilot binary; empty is an error (never the SDK's bundled runtime)
	Env              []string // KEY=VALUE BYOK block
	Model            string   // Copilot-catalog id; leave empty on BYOK (COPILOT_MODEL carries it)
	ReasoningEffort  string
	WorkingDirectory string
	AllowAllTools    bool
	UseLoggedInUser  bool // always explicit: unset, the SDK silently falls back to the GitHub subscription
	GitHubToken      string
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
	return &copilot.ClientOptions{
		Connection:       copilot.StdioConnection{Path: o.CLIPath, Env: append(os.Environ(), o.Env...)},
		WorkingDirectory: o.WorkingDirectory,
		UseLoggedInUser:  copilot.Bool(o.UseLoggedInUser),
		GitHubToken:      o.GitHubToken,
	}, nil
}

// BuildSessionConfig denies tool calls unless AllowAllTools is set.
func BuildSessionConfig(o Options) *copilot.SessionConfig {
	handler := denyAllTools
	if o.AllowAllTools {
		handler = approveToolOnce
	}
	return &copilot.SessionConfig{
		ClientName:          DefaultClientName,
		Model:               o.Model,
		ReasoningEffort:     o.ReasoningEffort,
		WorkingDirectory:    o.WorkingDirectory,
		Provider:            providerFromEnv(o.Env),
		OnPermissionRequest: handler,
	}
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

// Ask runs one prompt to completion; assistant events arrive on the SDK goroutine, hence the mutex.
func Ask(ctx context.Context, o Options, prompt string) (Result, error) {
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
	defer client.Stop()

	session, err := client.CreateSession(ctx, BuildSessionConfig(o))
	if err != nil {
		return Result{}, fmt.Errorf("create session: %w", err)
	}
	defer session.Disconnect()

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

	start := time.Now()
	if _, err := session.SendAndWait(ctx, copilot.MessageOptions{Prompt: prompt}); err != nil {
		return Result{}, fmt.Errorf("send prompt: %w", err)
	}
	mu.Lock()
	defer mu.Unlock()
	return Result{Text: strings.TrimSpace(text.String()), SessionID: session.SessionID, Model: model, Duration: time.Since(start)}, nil
}
