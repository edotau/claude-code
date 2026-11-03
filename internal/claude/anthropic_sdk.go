// anthropic_sdk.go — the one place claude calls github.com/anthropics/anthropic-sdk-go: a streaming Messages call.
package claude

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/edotau/claude-code/internal/textutil"
)

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
