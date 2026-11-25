// Package jev asks the local Open-Jev classifier which model tier a brief needs; a shadow signal, never a router input.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/proc"
)

const (
	defaultURL = "http://127.0.0.1:11401"
	urlEnv     = "HARNESS_JEV_URL"
	maxBrief   = 3500
	timeout    = 2 * time.Second
	maxBody    = 1 << 16
)

// Error classes recorded on a dispatch; never a URL or body.
const (
	ClassDown        = "down"
	ClassTimeout     = "timeout"
	ClassBadResponse = "bad-response"
	ClassOff         = "off"
)

// Typed errors, one per class.
var (
	ErrOff         = errors.New("jev: off")
	ErrDown        = errors.New("jev: server down")
	ErrTimeout     = errors.New("jev: timeout")
	ErrBadResponse = errors.New("jev: bad response")
)

// Result is the tier Open-Jev picked, its top probability and the call's duration.
type Result struct {
	Pick       string
	Confidence float64
	Ms         int64
}

// Class is the short error class of err, "" for nil.
func Class(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrOff):
		return ClassOff
	case errors.Is(err, ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return ClassTimeout
	case errors.Is(err, ErrBadResponse):
		return ClassBadResponse
	}
	return ClassDown
}

type answer struct {
	Choice        string             `json:"choice"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// Tier classifies the first 3500 characters of brief; it always returns within 2 s.
func Tier(ctx context.Context, brief string) (Result, error) {
	base := strings.TrimRight(proc.Value(ctx, urlEnv), "/")
	if base == "off" || base == "" && testing.Testing() { // a test binary never reaches the real server
		return Result{}, ErrOff
	}
	if base == "" {
		base = defaultURL
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	body, err := post(ctx, base+"/v1/systemone", request(brief))
	res := Result{Ms: time.Since(start).Milliseconds()}
	if err != nil {
		return res, err
	}
	a, err := parse(body)
	if err != nil {
		return res, err
	}
	for _, p := range a.Probabilities {
		res.Confidence = max(res.Confidence, p)
	}
	res.Pick = a.Choice
	return res, nil
}

func request(brief string) []byte {
	if r := []rune(brief); len(r) > maxBrief {
		brief = string(r[:maxBrief])
	}
	b, _ := json.Marshal(map[string]any{"state": brief, "questions": map[string]json.RawMessage{"tier": assets.JevTier()}})
	return b
}

func post(ctx context.Context, url string, payload []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, ErrDown
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ErrTimeout
		}
		return nil, ErrDown
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ErrTimeout
		}
		return nil, ErrBadResponse
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d", ErrBadResponse, resp.StatusCode)
	}
	return body, nil
}

func parse(body []byte) (answer, error) {
	var out struct {
		Answers struct {
			Tier answer `json:"tier"`
		} `json:"answers"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return answer{}, ErrBadResponse
	}
	a := out.Answers.Tier
	switch a.Choice {
	case "fast", "standard", "deep":
	default:
		return answer{}, ErrBadResponse
	}
	if len(a.Probabilities) == 0 {
		return answer{}, ErrBadResponse
	}
	for _, p := range a.Probabilities {
		if p < 0 || p > 1 {
			return answer{}, ErrBadResponse
		}
	}
	return a, nil
}
