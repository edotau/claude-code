package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/tokenledger"
)

// teeReadCloser lets a response body be read through a tee without losing its own Close.
type teeReadCloser struct {
	io.Reader
	io.Closer
}

// ledgerUsage is the token counts scraped from a provider's raw bytes for the usage ledger.
type ledgerUsage struct{ input, output, cacheRead int }

// usageBlockRE matches one `"usage":{...}` object with at most one nested level (prompt_tokens_details).
var usageBlockRE = regexp.MustCompile(`"usage"\s*:\s*\{(?:[^{}]|\{[^{}]*\})*\}`)

// wrapUsage turns a matched `"usage":{...}` pair into a standalone JSON object.
func wrapUsage(m []byte) []byte {
	_, v, _ := bytes.Cut(m, []byte(":"))
	return v
}

// scrapeChatUsage reads the last OpenAI-dialect usage object; input excludes cached tokens, as translate does.
func scrapeChatUsage(body []byte) ledgerUsage {
	return scrapeLastUsage(body, "prompt_tokens", "completion_tokens", "prompt_tokens_details")
}

// scrapeLastUsage parses the last usage object using the dialect's field names; input excludes cached tokens.
func scrapeLastUsage(body []byte, inKey, outKey, detailsKey string) ledgerUsage {
	matches := usageBlockRE.FindAll(body, -1)
	if len(matches) == 0 {
		return ledgerUsage{}
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(wrapUsage(matches[len(matches)-1]), &raw) != nil {
		return ledgerUsage{}
	}
	var details struct {
		Cached int `json:"cached_tokens"`
	}
	_ = json.Unmarshal(raw[detailsKey], &details)
	var u ledgerUsage
	if json.Unmarshal(raw[inKey], &u.input) != nil || json.Unmarshal(raw[outKey], &u.output) != nil {
		return ledgerUsage{}
	}
	if details.Cached > 0 && details.Cached <= u.input {
		u.cacheRead = details.Cached
		u.input -= u.cacheRead
	}
	return u
}

// scrapeAnthropicUsage takes each field's max across usage objects: stream usage is cumulative, so summing double-counts.
func scrapeAnthropicUsage(body []byte) ledgerUsage {
	var u ledgerUsage
	for _, m := range usageBlockRE.FindAll(body, -1) {
		var raw struct {
			Input     int `json:"input_tokens"`
			Output    int `json:"output_tokens"`
			CacheRead int `json:"cache_read_input_tokens"`
		}
		if json.Unmarshal(wrapUsage(m), &raw) == nil {
			u.input = max(u.input, raw.Input)
			u.output = max(u.output, raw.Output)
			u.cacheRead = max(u.cacheRead, raw.CacheRead)
		}
	}
	return u
}

// metered is every row the proxy bills per token; a cli passthrough rides the client's subscription and bills nothing.
func metered(p *providers.Provider) bool { return p.Auth.Method() != providers.AuthCLI }

// logLedgerUsage records one metered request to the usage ledger; best-effort, zero counts skipped.
func logLedgerUsage(in *http.Request, p *providers.Provider, model string, u ledgerUsage) {
	if u.input == 0 && u.output == 0 && u.cacheRead == 0 {
		return
	}
	session := in.Header.Get("X-Claude-Code-Session-Id")
	tokenledger.Append(tokenledger.Entry{
		Ts: time.Now(), Profile: p.Name, Provider: p.Provider, Model: model,
		Agent: in.Header.Get(proxy.AgentHeader), Session: session,
		Input: u.input, Output: u.output, CacheRead: u.cacheRead,
	})
}
