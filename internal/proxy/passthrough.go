package proxy

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/translate"
)

// noForward are inbound credentials and transport headers; egress auth is re-added by send.
var noForward = map[string]bool{
	"Authorization": true, "X-Api-Key": true, "X-Goog-Api-Key": true, "Host": true, "Accept-Encoding": true,
}

// parsePassthrough splits /p/<provider>/<openai|responses|gemini>/<rest>.
func parsePassthrough(path string) (provider, dialect, rest string, ok bool) {
	tail, ok := strings.CutPrefix(path, "/p/")
	if !ok {
		return "", "", "", false
	}
	parts := strings.SplitN(tail, "/", 3)
	if len(parts) < 3 || parts[0] == "" || parts[2] == "" {
		return "", "", "", false
	}
	switch parts[1] {
	case providers.ProtocolOpenAI, providers.ProtocolResponses, providers.ProtocolGemini:
		return parts[0], parts[1], parts[2], true
	}
	return "", "", "", false
}

// passthrough forwards a same-dialect request verbatim to the provider's route with egress auth.
func (r *Router) passthrough(w http.ResponseWriter, in *http.Request, provider, dialect, rest string) {
	start := r.now()
	sw := &statusWriter{ResponseWriter: w, rc: http.NewResponseController(w)}
	var model, extra string
	attempts := 0
	in = in.WithContext(context.WithValue(in.Context(), attemptsKey{}, &attempts))
	defer func() {
		fmt.Fprintf(r.log, "%s %s %s %d %d %s%s\n", start.Format(time.RFC3339), provider, cmp.Or(model, "-"), sw.status,
			attempts, time.Since(start).Round(time.Millisecond), extra)
	}()
	if in.Method != http.MethodPost && in.Method != http.MethodGet {
		writeError(sw, http.StatusNotFound, "proxy: unsupported route "+in.Method+" "+in.URL.Path+" (passthrough takes GET or POST)")
		return
	}
	reg, err := r.registry()
	if err != nil {
		writeError(sw, http.StatusInternalServerError, "proxy: "+err.Error())
		return
	}
	p, err := reg.Get(provider)
	if err != nil {
		writeError(sw, http.StatusBadRequest, "proxy: "+err.Error())
		return
	}
	route, ok := p.Route(dialect)
	dbxGemini := false
	if !ok && dialect == providers.ProtocolGemini {
		route, ok = databricksGemini(p)
		dbxGemini = ok
	}
	if !ok {
		writeError(sw, http.StatusNotFound, "proxy: provider "+p.Name+" has no "+dialect+" route")
		return
	}
	if err := safeRest(rest); err != nil {
		writeError(sw, http.StatusBadRequest, err.Error())
		return
	}
	query := in.URL.RawQuery
	if dbxGemini {
		if rest, query, model, err = geminiGatewayCall(p, rest, in.URL.Query()); err != nil || in.Method != http.MethodPost {
			if err == nil {
				err = errors.New("proxy: databricks gemini route takes POST only")
			}
			writeError(sw, http.StatusNotFound, err.Error())
			return
		}
	}
	var body []byte
	if in.Method == http.MethodPost {
		body, err = io.ReadAll(io.LimitReader(in.Body, maxBody+1))
		if err != nil {
			writeError(sw, http.StatusBadRequest, "proxy: read body: "+err.Error())
			return
		}
		if len(body) > maxBody {
			writeError(sw, http.StatusRequestEntityTooLarge, "proxy: request body over 64MiB")
			return
		}
		model = cmp.Or(passthroughModel(body), model)
		if dbxGemini {
			body = dropOffThoughts(body)
		}
		if dialect == providers.ProtocolOpenAI && strings.HasSuffix(rest, "chat/completions") && translate.IsGeminiModel(model) {
			body = fixGeminiChat(body)
		}
	}
	target := route + "/" + rest
	if query != "" {
		target += "?" + query
	}
	resp, err := r.sendMethod(in, in.Method, p, target, body, func(h http.Header) {
		for k, vs := range in.Header {
			ck := http.CanonicalHeaderKey(k)
			if !hopHeaders[ck] && !noForward[ck] {
				h[ck] = append([]string(nil), vs...)
			}
		}
	})
	if err != nil {
		var failure *fallbackError
		if errors.As(err, &failure) && failure.auth {
			writeError(sw, http.StatusUnauthorized, fmt.Sprintf("proxy: %s: authentication failed; %s", p.Name, authHint(p)))
			return
		}
		writeError(sw, http.StatusBadGateway, "proxy: "+p.Name+": "+err.Error())
		return
	}
	extra = passthroughDiag(resp)
	var tail *tailBuffer
	if metered(p) && in.Method == http.MethodPost && resp.StatusCode == http.StatusOK && dialect != providers.ProtocolGemini {
		tail = &tailBuffer{}
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.TeeReader(resp.Body, tail), resp.Body}
	}
	relay(sw, resp)
	if tail != nil {
		logLedgerUsage(in, p, model, scrapeUsage(tail.buf))
	}
}

// fixGeminiChat drops stream_options and fills a missing thoughtSignature on assistant tool_calls; else body as is.
func fixGeminiChat(body []byte) []byte {
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return body
	}
	changed := false
	if _, ok := top["stream_options"]; ok {
		delete(top, "stream_options")
		changed = true
	}
	var msgs []map[string]json.RawMessage
	if json.Unmarshal(top["messages"], &msgs) == nil {
		for _, m := range msgs {
			var calls []map[string]json.RawMessage
			if json.Unmarshal(m["tool_calls"], &calls) != nil {
				continue
			}
			touched := false
			for _, c := range calls {
				if c == nil { // a null entry: nothing to sign, and writing to it would panic
					continue
				}
				_, sig := c["thoughtSignature"]
				_, extra := c["extra_content"]
				if !sig && !extra {
					c["thoughtSignature"] = json.RawMessage(`"` + translate.SkipThoughtSignature + `"`)
					touched = true
				}
			}
			if touched {
				m["tool_calls"], _ = json.Marshal(calls)
				changed = true
			}
		}
		if changed {
			top["messages"], _ = json.Marshal(msgs)
		}
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(top)
	if err != nil {
		return body
	}
	return out
}

// tailBuffer keeps the last tailCap bytes written, so a stream's closing usage chunk survives without buffering it all.
type tailBuffer struct{ buf []byte }

const tailCap = 256 << 10

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > 2*tailCap {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-tailCap:]...)
	}
	return len(p), nil
}

// scrapeUsage reads the last usage object in either dialect: chat prompt/completion or responses input/output tokens.
func scrapeUsage(body []byte) ledgerUsage {
	if u := scrapeChatUsage(body); u.input != 0 || u.output != 0 || u.cacheRead != 0 {
		return u
	}
	matches := usageBlockRE.FindAll(body, -1)
	if len(matches) == 0 {
		return ledgerUsage{}
	}
	var raw struct {
		Input   int `json:"input_tokens"`
		Output  int `json:"output_tokens"`
		Details *struct {
			Cached int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	}
	if json.Unmarshal(wrapUsage(matches[len(matches)-1]), &raw) != nil {
		return ledgerUsage{}
	}
	u := ledgerUsage{input: raw.Input, output: raw.Output}
	if raw.Details != nil && raw.Details.Cached > 0 && raw.Details.Cached <= u.input {
		u.cacheRead = raw.Details.Cached
		u.input -= u.cacheRead
	}
	return u
}

// attemptsKey carries a passthrough's upstream-attempt counter into sendMethod (auth refresh replays count).
type attemptsKey struct{}

// passthroughModel is the request body's "model", or "" when absent or not JSON.
func passthroughModel(body []byte) string {
	var head struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &head)
	return head.Model
}

// passthroughDiag is the log suffix: upstream request id, Retry-After and, on an error, its sanitized code.
func passthroughDiag(resp *http.Response) string {
	var b strings.Builder
	for _, k := range []string{"X-Request-Id", "X-Databricks-Request-Id", "X-Databricks-Requestid", "Request-Id"} {
		if v := resp.Header.Get(k); v != "" {
			b.WriteString(" req=" + diagToken(v))
			break
		}
	}
	if v := resp.Header.Get("Retry-After"); v != "" {
		b.WriteString(" retry_after=" + diagToken(v))
	}
	if resp.StatusCode >= 400 {
		peek, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(peek), resp.Body), resp.Body}
		var e struct {
			Code  string `json:"error_code"`
			Error struct {
				Code string `json:"code"`
				Type string `json:"type"`
			} `json:"error"`
		}
		_ = json.Unmarshal(peek, &e)
		if c := cmp.Or(e.Code, e.Error.Code, e.Error.Type); c != "" {
			b.WriteString(" err=" + diagToken(c))
		}
	}
	return b.String()
}

// diagToken keeps a log value to [A-Za-z0-9._-], at most 64 chars, so a header can't inject text into the log.
func diagToken(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '.' || r == '_' || r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return r
		}
		return -1
	}, s)
	return s[:min(len(s), 64)]
}

// databricksGemini is a Databricks row's native Gemini base: its anthropic base with /gemini in place of /anthropic.
func databricksGemini(p *providers.Provider) (string, bool) {
	if p.Preset != providers.PresetDatabricks {
		return "", false
	}
	if base, ok := strings.CutSuffix(strings.TrimRight(p.BaseURL, "/"), "/anthropic"); ok {
		return base + "/gemini", true
	}
	if p.Host == "" {
		return "", false
	}
	if p.IsAIGateway() {
		return strings.TrimRight(p.Host, "/") + "/ai-gateway/gemini", true
	}
	return strings.TrimRight(p.Host, "/") + "/serving-endpoints/gemini", true
}

// safeRest rejects a passthrough tail that could climb out of the route: dot segments, empty segments, escapes.
func safeRest(rest string) error {
	if strings.ContainsAny(rest, "\\%\x00") || strings.Contains(rest, "//") || strings.HasPrefix(rest, "/") {
		return errors.New("proxy: passthrough path must be plain segments")
	}
	for seg := range strings.SplitSeq(rest, "/") {
		if seg == "." || seg == ".." {
			return errors.New("proxy: passthrough path must not contain dot segments")
		}
	}
	return nil
}

// geminiCallRE is the one call agy's gateway mode makes: v1beta/models/<id>:(stream)GenerateContent.
var geminiCallRE = regexp.MustCompile(`^v1beta/models/([A-Za-z0-9._-]+):(generateContent|streamGenerateContent)$`)

// geminiGatewayCall maps an allowed call onto the row's wire id; the query keeps only alt=sse.
func geminiGatewayCall(p *providers.Provider, rest string, q url.Values) (path, query, model string, err error) {
	m := geminiCallRE.FindStringSubmatch(rest)
	if m == nil {
		return "", "", "", errors.New("proxy: databricks gemini route serves only v1beta/models/<id>:(stream)GenerateContent")
	}
	model = p.WireModel(m[1])
	if q.Get("alt") == "sse" {
		query = "alt=sse"
	}
	return "v1beta/models/" + model + ":" + m[2], query, model, nil
}

// dropOffThoughts removes includeThoughts when thinkingBudget is 0: Vertex rejects thoughts with thinking off.
func dropOffThoughts(body []byte) []byte {
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return body
	}
	var gen map[string]json.RawMessage
	if json.Unmarshal(top["generationConfig"], &gen) != nil {
		return body
	}
	var think map[string]json.RawMessage
	if json.Unmarshal(gen["thinkingConfig"], &think) != nil {
		return body
	}
	var budget float64
	if _, ok := think["includeThoughts"]; !ok || json.Unmarshal(think["thinkingBudget"], &budget) != nil || budget != 0 {
		return body
	}
	delete(think, "includeThoughts")
	gen["thinkingConfig"], _ = json.Marshal(think)
	top["generationConfig"], _ = json.Marshal(gen)
	out, err := json.Marshal(top)
	if err != nil {
		return body
	}
	return out
}
