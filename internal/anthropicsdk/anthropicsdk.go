// Package anthropicsdk boxes github.com/anthropics/anthropic-sdk-go: one streaming Messages call, stdlib types out.
package anthropicsdk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// Request is one single-turn ask. Authorize sets auth headers per request; the SDK's env credential chain is off.
type Request struct {
	BaseURL   string
	Model     string
	Prompt    string
	Effort    string // low|medium|high|xhigh|max; "" = model default
	MaxTokens int64
	Authorize func(context.Context, http.Header) error
}

// Response is the served model and token usage.
type Response struct {
	Model        string
	InputTokens  int64
	OutputTokens int64
}

// APIError is an HTTP failure with the status and Retry-After the caller's retry loop needs.
type APIError struct {
	Status int
	After  time.Duration
	msg    string
}

func (e *APIError) Error() string             { return e.msg }
func (e *APIError) StatusCode() int           { return e.Status }
func (e *APIError) RetryAfter() time.Duration { return e.After }

// Stream sends r and writes text deltas to w as they arrive.
func Stream(ctx context.Context, r Request, w io.Writer) (Response, error) {
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
	var res Response
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
	return res, boxError(stream.Err())
}

func boxError(err error) error {
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) {
		return err
	}
	out := &APIError{Status: apiErr.StatusCode, msg: fmt.Sprintf("HTTP %d: %s", apiErr.StatusCode, apiErr.RawJSON())}
	if apiErr.Response != nil {
		if s, perr := strconv.Atoi(apiErr.Response.Header.Get("Retry-After")); perr == nil {
			out.After = time.Duration(s) * time.Second
		}
	}
	return out
}
