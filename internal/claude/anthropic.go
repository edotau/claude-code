// the one place claude calls github.com/anthropics/anthropic-sdk-go: a streaming Messages call.
package claude

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/shapes"
	"github.com/edotau/claude-code/internal/textutil"
)

// Anthropic auth modes for the `anthropic` verb.
const (
	AuthLogin  = "login"     // claude.ai login (provider subscription)
	AuthOAuth  = "oauth"     // CLAUDE_CODE_OAUTH_TOKEN from the vault
	AuthAPIKey = "x-api-key" // provider anthropic, ANTHROPIC_API_KEY
)

// DefaultOAuthName and DefaultAPIKeyName are the vault keys used when a flag carries no =NAME.
const (
	DefaultOAuthName  = oauthKey
	DefaultAPIKeyName = "ANTHROPIC_API_KEY"
)

// AnthropicOptions are the launcher flags plus the verb's own auth selection.
type AnthropicOptions struct {
	agent.LaunchOptions
	Mode string // AuthLogin | AuthOAuth | AuthAPIKey
	Name string // vault key holding the secret ("" for AuthLogin)
}

var envNameRE = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// ParseAnthropicArgs strips --oauth[=NAME] and --x-api-key[=NAME] (a bare flag takes no value), then the shared launcher flags.
func ParseAnthropicArgs(args []string) (AnthropicOptions, []string, error) {
	o := AnthropicOptions{Mode: AuthLogin}
	var kept []string
	for i, a := range args {
		if a == "--" {
			kept = append(kept, args[i:]...)
			break
		}
		name, val, hasVal := strings.Cut(a, "=")
		if name != "--oauth" && name != "--x-api-key" {
			kept = append(kept, a)
			continue
		}
		mode, def := AuthOAuth, DefaultOAuthName
		if name == "--x-api-key" {
			mode, def = AuthAPIKey, DefaultAPIKeyName
		}
		if o.Mode == mode {
			return o, nil, fmt.Errorf("%s given more than once; pass it a single time", name)
		}
		if o.Mode != AuthLogin {
			return o, nil, errors.New("--oauth and --x-api-key cannot be combined; pick one auth mode")
		}
		if hasVal {
			if err := checkKeyName(name, val); err != nil {
				return o, nil, err
			}
			def = val
		}
		o.Mode, o.Name = mode, def
	}
	base, rest, err := agent.ParseArgs(kept, true)
	if err != nil {
		return o, nil, err
	}
	if base.Provider != "" {
		return o, nil, errors.New("anthropic picks its own profile; drop --profile (or use claude-code opus --profile ...)")
	}
	o.LaunchOptions = base
	return o, rest, nil
}

// checkKeyName accepts only an env-var-shaped vault key name; a credential-shaped value is refused unechoed.
func checkKeyName(flag, v string) error {
	if shapes.Looks(v) {
		return fmt.Errorf("%s=NAME takes a vault key name ([A-Z_][A-Z0-9_]*); store secrets with claude-code vault set", flag)
	}
	if !envNameRE.MatchString(v) {
		return fmt.Errorf("%s=NAME takes a vault key name ([A-Z_][A-Z0-9_]*); store secrets with claude-code vault set", flag)
	}
	return nil
}

// Anthropic resolves a first-party Claude Code launch; secret is the value for the oauth and x-api-key modes.
func Anthropic(ctx context.Context, o AnthropicOptions, args []string, secret string) (agent.Plan, error) {
	opts := o.LaunchOptions
	reg, err := providers.Load()
	if err != nil {
		return agent.Plan{}, err
	}
	method := providers.AuthCLI
	if o.Mode == AuthAPIKey {
		method = providers.AuthKey
	}
	if opts.Provider = anthropicProfile(reg, method); opts.Provider == "" {
		return agent.Plan{}, fmt.Errorf("no anthropic profile with %s auth in the registry", method)
	}
	plan, err := Claude(ctx, opts, args)
	if err != nil || plan.ForeignBase { // a foreign shell endpoint never receives the vault secret
		return plan, err
	}
	switch o.Mode {
	case AuthOAuth:
		plan.Set[oauthKey] = secret
	case AuthAPIKey:
		if o.Name != DefaultAPIKeyName { // the apiKeyHelper reads the registry's env name, so carry this key directly
			plan.Set[DefaultAPIKeyName] = secret
			delete(plan.Set, "CLAUDE_CODE_API_KEY_HELPER_TTL_MS")
			if len(plan.Argv) >= 2 && plan.Argv[0] == "--settings" {
				plan.Argv = plan.Argv[2:]
			}
			plan.Files = nil
		}
	}
	return plan, nil
}

// anthropicProfile is the first anthropic-provider profile signing in by method ("" when none).
func anthropicProfile(reg *providers.Registry, method string) string {
	for _, n := range reg.ProfilesOf("anthropic") {
		if reg.Providers[n].Auth.Method() == method {
			return n
		}
	}
	return ""
}

// apiRequest is one single-turn ask. Authorize sets auth headers per request; the SDK's env credential chain is off.
type apiRequest struct {
	BaseURL   string
	Model     string
	Prompt    string
	Effort    string // low|medium|high|xhigh|max; "" = model default
	MaxTokens int64
	Authorize func(context.Context, http.Header) error
}

// apiResponse is the served model and token usage.
type apiResponse struct {
	Model        string
	InputTokens  int64
	OutputTokens int64
}

// apiError is an HTTP failure with the status and Retry-After the caller's retry loop needs.
type apiError struct {
	Status int
	After  time.Duration
	msg    string
}

func (e *apiError) Error() string             { return e.msg }
func (e *apiError) StatusCode() int           { return e.Status }
func (e *apiError) RetryAfter() time.Duration { return e.After }

// streamMessages sends r and writes text deltas to w as they arrive.
func streamMessages(ctx context.Context, r apiRequest, w io.Writer) (apiResponse, error) {
	client := anthropic.NewClient(
		option.WithoutEnvironmentDefaults(),
		option.WithBaseURL(r.BaseURL),
		option.WithMaxRetries(0), // the caller's dispatch loop owns retries
		option.WithMiddleware(func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
			req.Header.Del("X-Api-Key")
			req.Header.Del("Authorization")
			if r.Authorize != nil {
				if err := r.Authorize(req.Context(), req.Header); err != nil {
					return nil, err
				}
			}
			return next(req)
		}),
	)
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(r.Model),
		MaxTokens: r.MaxTokens,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(r.Prompt))},
	}
	if r.Effort != "" {
		params.OutputConfig = anthropic.OutputConfigParam{Effort: anthropic.OutputConfigEffort(r.Effort)}
	}
	var res apiResponse
	stream := client.Messages.NewStreaming(ctx, params)
	defer stream.Close()
	for stream.Next() {
		switch ev := stream.Current().AsAny().(type) {
		case anthropic.MessageStartEvent:
			res.Model = string(ev.Message.Model)
			res.InputTokens, res.OutputTokens = ev.Message.Usage.InputTokens, ev.Message.Usage.OutputTokens
		case anthropic.ContentBlockDeltaEvent:
			if t, ok := ev.Delta.AsAny().(anthropic.TextDelta); ok {
				if _, err := io.WriteString(w, t.Text); err != nil {
					return res, err
				}
			}
		case anthropic.MessageDeltaEvent:
			if ev.Usage.InputTokens > 0 { // cumulative; input may be absent mid-stream
				res.InputTokens = ev.Usage.InputTokens
			}
			res.OutputTokens = ev.Usage.OutputTokens
		}
	}
	return res, boxSDKError(stream.Err())
}

func boxSDKError(err error) error {
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) {
		return err
	}
	out := &apiError{Status: apiErr.StatusCode, msg: fmt.Sprintf("HTTP %d: %s", apiErr.StatusCode, apiErr.RawJSON())}
	if apiErr.Response != nil {
		// Keep the SDK caller's integer-only header policy.
		v := apiErr.Response.Header.Get("Retry-After")
		if v == strings.TrimSpace(v) && !strings.Contains(v, ":") {
			out.After, _ = textutil.ParseRetryAfter(v, time.Now())
		}
	}
	return out
}
