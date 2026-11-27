package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/platform/tokenledger"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/proxy/translate"
	"github.com/edotau/claude-code/internal/routing/catalog"
	"github.com/edotau/claude-code/internal/routing/models"
	"github.com/edotau/claude-code/internal/routing/providers"
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
	m, _ := in.Context().Value(metaKey{}).(*reqMeta)
	requested := m.plainModel()
	if !swapped(requested, model) {
		requested = ""
	}
	now := time.Now()
	total, first := ledgerDurations(m, now)
	tokenledger.Append(tokenledger.Entry{
		Ts: now, Profile: p.Name, Provider: p.Provider, Model: model, Requested: requested,
		Agent: in.Header.Get(proxy.AgentHeader), Session: session,
		AskedModel: m.askedModel(), Call: string(m.callKind()), Route: m.routeIfDiffers(p.Name),
		Input: u.input, Output: u.output, CacheRead: u.cacheRead,
		CacheWrite: u.cacheWrite, CacheWrite1h: u.cacheWrite1h,
		FirstMs: first, TotalMs: total, CostUSD: ledgerCost(p, model, u, now),
	})
}

// ledgerDurations is the attempt's total and first-byte milliseconds; 0 when unknown.
func ledgerDurations(m *reqMeta, now time.Time) (total, first int64) {
	if m != nil && !m.sent.IsZero() {
		total = now.Sub(m.sent).Milliseconds()
		if m.first.After(m.sent) {
			first = m.first.Sub(m.sent).Milliseconds()
		}
	}
	return total, first
}

// listTable is prices.json, re-read when its mtime moves, stat'ed at most once a minute.
var listTable struct {
	sync.Mutex
	t            catalog.Prices
	path         string
	mod, checked time.Time
}

// listPrices is the cached list table; a failed stat or read keeps the last one.
func listPrices(now time.Time) catalog.Prices {
	listTable.Lock()
	defer listTable.Unlock()
	path := catalog.PricesPath()
	if path == listTable.path && !listTable.checked.IsZero() && now.Sub(listTable.checked) < time.Minute {
		return listTable.t
	}
	if path != listTable.path {
		listTable.t, listTable.mod, listTable.path = catalog.Prices{}, time.Time{}, path
	}
	listTable.checked = now
	if fi, err := os.Stat(path); err == nil && !fi.ModTime().Equal(listTable.mod) {
		if t, err := catalog.ReadPrices(); err == nil {
			listTable.t, listTable.mod = t, fi.ModTime()
		}
	}
	return listTable.t
}

// ledgerCost is u at p's owner price for model, else the list price; 0 when neither names one.
func ledgerCost(p *providers.Provider, model string, u ledgerUsage, now time.Time) float64 {
	price, ok := p.OwnerPrice(model)
	if !ok {
		list := listPrices(now)
		if price, ok = list.ListPrice(p.Provider, model); !ok { // the served spelling first: rows may differ from the key's
			price, ok = list.ListPrice(p.Provider, models.ModelKey(model))
		}
	}
	if !ok {
		return 0
	}
	return price.USD(u.input, u.output, u.cacheRead, u.cacheWrite, u.cacheWrite1h)
}

// callKind is the typed shape of a request, decided from its path and, for messages, the body fields already parsed.
type callKind string

const (
	callMessages    callKind = "messages"
	callTiny        callKind = "messages_tiny" // a messages call with no tools and max_tokens <= tinyMaxTokens
	callCountTokens callKind = "count_tokens"
	callResponses   callKind = "responses"
	callChat        callKind = "chat"
	callGemini      callKind = "gemini"
	callOther       callKind = "other"
)

// tinyMaxTokens bounds a tool-less tiny call; Claude Code's auto-mode classifier stage 1 is one, but so can others be.
const tinyMaxTokens = 64

// maxModelToken caps the client-supplied model string kept in the ledger and log.
const maxModelToken = 128

// reqMeta is what one request tells the ledger and log beyond its mapped model; the proxy sets it once per request.
type reqMeta struct {
	route string   // the /p/<profile> the client asked for
	asked string   // the body's model string, sanitized and capped
	plain string   // the asked model without a [1m] marker, for the swap check
	call  callKind // the request kind

	sent, first time.Time // the serving attempt's send and its first client byte
}

type metaKey struct{}

func withMeta(in *http.Request, m *reqMeta) *http.Request {
	return in.WithContext(context.WithValue(in.Context(), metaKey{}, m))
}

func (m *reqMeta) plainModel() string {
	if m == nil {
		return ""
	}
	return m.plain
}

func (m *reqMeta) askedModel() string {
	if m == nil {
		return ""
	}
	return m.asked
}

func (m *reqMeta) callKind() callKind {
	if m == nil {
		return ""
	}
	return m.call
}

// routeIfDiffers is the asked /p/<profile> only when another profile served the request.
func (m *reqMeta) routeIfDiffers(served string) string {
	if m == nil || m.route == served {
		return ""
	}
	return m.route
}

// noteBody records the body's model string and, for a messages call, whether the body is tool-less and tiny.
func (m *reqMeta) noteBody(model string, top map[string]json.RawMessage) {
	m.asked = modelToken(model)
	if m.call == callMessages && isTiny(top) {
		m.call = callTiny
	}
}

// isTiny reports a body with no tools and a positive max_tokens of at most tinyMaxTokens.
func isTiny(top map[string]json.RawMessage) bool {
	if t := bytes.TrimSpace(top["tools"]); len(t) > 0 && string(t) != "null" {
		if j := skipSpace(t, 1); t[0] != '[' || j >= len(t) || t[j] != ']' {
			return false
		}
	}
	n, err := strconv.Atoi(string(bytes.TrimSpace(top["max_tokens"])))
	return err == nil && n > 0 && n <= tinyMaxTokens
}

// diag is the log tokens for the request: asked_model, call, and route when another profile served it.
func (m *reqMeta) diag(served string) string {
	if m == nil {
		return ""
	}
	s := ""
	if m.asked != "" {
		s = " asked_model=" + m.asked
	}
	if m.call != "" {
		s += " call=" + string(m.call)
	}
	if r := m.routeIfDiffers(served); r != "" {
		s += " route=" + r
	}
	return s
}

// modelToken drops control, space and non-printable runes from client input and caps it at maxModelToken bytes.
func modelToken(s string) string {
	clean := len(s) <= maxModelToken
	for _, c := range s {
		if !unicode.IsPrint(c) || unicode.IsSpace(c) {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	for _, c := range s {
		if b.Len()+utf8.RuneLen(c) > maxModelToken {
			break
		}
		if unicode.IsPrint(c) && !unicode.IsSpace(c) {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// passthroughCall is the call kind of a passthrough request from its dialect and path.
func passthroughCall(dialect, path string) callKind {
	switch dialect {
	case providers.ProtocolResponses:
		if path == "responses" || strings.HasSuffix(path, "/responses") {
			return callResponses
		}
	case providers.ProtocolGemini:
		return callGemini
	case providers.ProtocolOpenAI:
		if isChatCompletion(dialect, path) {
			return callChat
		}
	}
	return callOther
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
