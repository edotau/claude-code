package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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

// usageObjects returns every complete JSON object value of key in body, in order; a cut or malformed one is skipped.
func usageObjects(body []byte, key string) [][]byte {
	needle := []byte(`"` + key + `"`)
	var out [][]byte
	for off := 0; ; {
		i := bytes.Index(body[off:], needle)
		if i < 0 {
			return out
		}
		j := skipSpace(body, off+i+len(needle))
		if off = j; j >= len(body) || body[j] != ':' {
			continue
		}
		if j = skipSpace(body, j+1); j >= len(body) || body[j] != '{' {
			continue
		}
		if end := objectEnd(body[j:min(len(body), j+usageObjectCap)]); end > 0 {
			out, off = append(out, body[j:j+end]), j+end
		}
	}
}

// usageObjectCap bounds one usage object (real ones are under 2 KB), so a cut one costs O(cap), not O(body).
const usageObjectCap = 16 << 10

func skipSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\r' || b[i] == '\n') {
		i++
	}
	return i
}

// objectEnd is the length of the JSON object starting at b[0], strings respected; -1 when it is cut off.
func objectEnd(b []byte) int {
	depth, inStr, esc := 0, false, false
	for i, c := range b {
		switch {
		case esc:
			esc = false
		case inStr:
			esc, inStr = c == '\\', c != '"'
		case c == '"':
			inStr = true
		case c == '{':
			depth++
		case c == '}':
			if depth--; depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

// scrapeChatUsage reads the last OpenAI-dialect usage object; input excludes cached tokens, as translate does.
func scrapeChatUsage(body []byte) ledgerUsage {
	return scrapeLastUsage(body, "prompt_tokens", "completion_tokens", "prompt_tokens_details")
}

// scrapeLastUsage parses the last usage object using the dialect's field names; input excludes cache reads and writes.
func scrapeLastUsage(body []byte, inKey, outKey, detailsKey string) ledgerUsage {
	matches := usageObjects(body, "usage")
	if len(matches) == 0 {
		return ledgerUsage{}
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(matches[len(matches)-1], &raw) != nil {
		return ledgerUsage{}
	}
	var details struct {
		Cached int `json:"cached_tokens"`
		Write  int `json:"cache_write_tokens"`
	}
	_ = json.Unmarshal(raw[detailsKey], &details)
	// An absent or null count is 0: an embeddings usage has no completion_tokens, and the prompt count still counts.
	input, cacheRead := translate.SplitCached(count(raw[inKey]), details.Cached)
	input, cacheWrite := translate.SplitCached(input, details.Write)
	return ledgerUsage{input: input, output: count(raw[outKey]), cacheRead: cacheRead, cacheWrite: cacheWrite}
}

// geminiUsage is one usageMetadata object; promptTokenCount includes cachedContentTokenCount.
type geminiUsage struct {
	Prompt     int `json:"promptTokenCount"`
	ToolPrompt int `json:"toolUsePromptTokenCount"`
	Candidates int `json:"candidatesTokenCount"`
	Thoughts   int `json:"thoughtsTokenCount"`
	Cached     int `json:"cachedContentTokenCount"`
}

// scrapeGeminiUsage takes each usageMetadata counter's own max: every stream chunk repeats the running count.
func scrapeGeminiUsage(body []byte) ledgerUsage {
	var m geminiUsage
	for _, obj := range usageObjects(body, "usageMetadata") {
		var u geminiUsage
		if json.Unmarshal(obj, &u) == nil {
			m = geminiUsage{max(m.Prompt, u.Prompt), max(m.ToolPrompt, u.ToolPrompt), max(m.Candidates, u.Candidates),
				max(m.Thoughts, u.Thoughts), max(m.Cached, u.Cached)}
		}
	}
	input, cacheRead := translate.SplitCached(m.Prompt+m.ToolPrompt, m.Cached)
	return ledgerUsage{input: input, output: m.Candidates + m.Thoughts, cacheRead: cacheRead}
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
	for _, m := range usageObjects(body, "usage") {
		var raw struct {
			Input      int `json:"input_tokens"`
			Output     int `json:"output_tokens"`
			CacheRead  int `json:"cache_read_input_tokens"`
			CacheWrite int `json:"cache_creation_input_tokens"`
			Creation   struct {
				OneHour int `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
		}
		if json.Unmarshal(m, &raw) == nil {
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
	return " asked=" + agentToken(asked)
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
