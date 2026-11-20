package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/proxy/translate"
	"github.com/edotau/claude-code/internal/tokenledger"
)

// teeReadCloser lets a response body be read through a tee without losing its own Close.
type teeReadCloser struct {
	io.Reader
	io.Closer
}

// ledgerUsage is the token counts scraped from a provider's raw bytes for the usage ledger.
type ledgerUsage struct{ input, output, cacheRead, cacheWrite, cacheWrite1h int }

// usageBlockRE matches one `"usage":{...}` object with at most one nested level (prompt_tokens_details).
var usageBlockRE = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`"usage"\s*:\s*\{(?:[^{}]|\{[^{}]*\})*\}`) })

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
	matches := usageBlockRE().FindAll(body, -1)
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
	// An absent or null count is 0: an embeddings usage has no completion_tokens, and the prompt count still counts.
	prompt := count(raw[inKey])
	input, cacheRead := translate.SplitCached(prompt, details.Cached)
	return ledgerUsage{input: input, output: count(raw[outKey]), cacheRead: cacheRead}
}

// count decodes one token count; absent, null or non-numeric reads as 0.
func count(raw json.RawMessage) int {
	var n int
	if json.Unmarshal(raw, &n) != nil {
		return 0
	}
	return n
}

// scrapeAnthropicUsage takes each field's max across usage objects: stream usage is cumulative, a sum double-counts.
func scrapeAnthropicUsage(body []byte) ledgerUsage {
	var u ledgerUsage
	for _, m := range usageBlockRE().FindAll(body, -1) {
		var raw struct {
			Input      int `json:"input_tokens"`
			Output     int `json:"output_tokens"`
			CacheRead  int `json:"cache_read_input_tokens"`
			CacheWrite int `json:"cache_creation_input_tokens"`
			Creation   struct {
				OneHour int `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
		}
		if json.Unmarshal(wrapUsage(m), &raw) == nil {
			u.input = max(u.input, raw.Input)
			u.output = max(u.output, raw.Output)
			u.cacheRead = max(u.cacheRead, raw.CacheRead)
			u.cacheWrite = max(u.cacheWrite, raw.CacheWrite)
			u.cacheWrite1h = max(u.cacheWrite1h, raw.Creation.OneHour)
		}
	}
	return u
}

// metered is every row the proxy bills per token; a cli passthrough rides the client's subscription and bills nothing.
func metered(p *providers.Provider) bool { return p.Auth.Method() != providers.AuthCLI }

// logLedgerUsage records one metered request to the usage ledger; best-effort, zero counts skipped.
func logLedgerUsage(in *http.Request, p *providers.Provider, model string, u ledgerUsage) {
	if u.input == 0 && u.output == 0 && u.cacheRead == 0 && u.cacheWrite == 0 {
		return
	}
	session := in.Header.Get("X-Claude-Code-Session-Id")
	requested, _ := in.Context().Value(askedKey{}).(string)
	if !swapped(requested, model) {
		requested = ""
	}
	tokenledger.Append(tokenledger.Entry{
		Ts: time.Now(), Profile: p.Name, Provider: p.Provider, Model: model, Requested: requested,
		Agent: in.Header.Get(proxy.AgentHeader), Session: session,
		Input: u.input, Output: u.output, CacheRead: u.cacheRead,
		CacheWrite: u.cacheWrite, CacheWrite1h: u.cacheWrite1h,
	})
}

// askedKey carries the model id the client asked for, before any rewrite, to the ledger write.
type askedKey struct{}

func withAsked(in *http.Request, id string) *http.Request {
	return in.WithContext(context.WithValue(in.Context(), askedKey{}, id))
}

// swapped reports that served is a different model from asked; spellings of one model (prefix, [1m], alias) are not.
func swapped(asked, served string) bool {
	return asked != "" && asked != served && models.ModelKey(asked) != models.ModelKey(served)
}

// reqDiag is the log token naming the asked model, only when the request was served by another one.
func reqDiag(asked, served string) string {
	if !swapped(asked, served) {
		return ""
	}
	return " req=" + agentToken(asked)
}

// swapLogEvery is the least gap between two log lines for one (profile, asked, served) swap.
const swapLogEvery = time.Minute

// noteSwap logs a silent model swap once per swapLogEvery per (profile, asked, served).
func (r *Router) noteSwap(profile, asked, served, why string) {
	key := strings.Join([]string{profile, asked, served}, "\x00")
	now := r.now()
	r.swapMu.Lock()
	if r.swapSeen == nil {
		r.swapSeen = map[string]time.Time{}
	}
	last, seen := r.swapSeen[key]
	if seen && now.Sub(last) < swapLogEvery {
		r.swapMu.Unlock()
		return
	}
	r.swapSeen[key] = now
	r.swapMu.Unlock()
	fmt.Fprintf(r.log, "proxy: %s asked for %s, serving %s (%s)\n", profile, asked, served, why)
}
