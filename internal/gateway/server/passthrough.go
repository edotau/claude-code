package server

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
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/edotau/claude-code/internal/gateway/translate"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
)

// noForward are inbound credentials and transport headers; egress auth is re-added by send.
var noForward = map[string]bool{
	"Authorization": true, "X-Api-Key": true, "X-Goog-Api-Key": true, "Host": true, "Accept-Encoding": true,
}

// parsePassthrough splits /p/<provider>/<openai|responses|gemini>/<rest>.
func parsePassthrough(path string) (provider, dialect, rest string, ok bool) {
	provider, tail, ok := parseRoute(path)
	if !ok {
		return "", "", "", false
	}
	dialect, rest, found := strings.Cut(tail, "/")
	if !found || provider == "" || rest == "" {
		return "", "", "", false
	}
	switch dialect {
	case providers.ProtocolOpenAI, providers.ProtocolResponses, providers.ProtocolGemini:
		return provider, dialect, rest, true
	}
	return "", "", "", false
}

// passthrough forwards a same-dialect request to the provider's route with egress auth and model body fixes.
func (r *Router) passthrough(w http.ResponseWriter, in *http.Request, provider, dialect, rest string) {
	start := r.now()
	sw := &statusWriter{ResponseWriter: w, rc: http.NewResponseController(w)}
	var model, extra string
	attempts := 0
	in = in.WithContext(context.WithValue(in.Context(), attemptsKey{}, &attempts))
	defer func() {
		fmt.Fprintf(r.log, "%s %s %s %d %d %s%s%s\n", start.Format(time.RFC3339), provider, cmp.Or(model, "-"), sw.status,
			attempts, time.Since(start).Round(time.Millisecond), viaDiag(in), extra)
	}()
	model, extra = r.forwardPassthrough(sw, in, provider, dialect, rest)
}

func (r *Router) forwardPassthrough(sw *statusWriter, in *http.Request,
	provider, dialect, rest string) (model, extra string) {
	if in.Method != http.MethodPost && in.Method != http.MethodGet {
		writeError(sw, http.StatusNotFound,
			"proxy: unsupported route "+in.Method+" "+in.URL.Path+" (passthrough takes GET or POST)")
		return
	}
	p, route, dbxGemini, ok := r.passthroughRoute(sw, provider, dialect)
	if !ok {
		return
	}
	var err error
	if err := safeRest(rest); err != nil {
		writeError(sw, http.StatusBadRequest, err.Error())
		return
	}
	query := in.URL.RawQuery
	if dbxGemini {
		if rest, query, model, err = geminiGatewayCall(p, rest, in.URL.Query()); err != nil ||
			in.Method != http.MethodPost {
			if err == nil {
				err = errors.New("proxy: databricks gemini route takes POST only")
			}
			writeError(sw, http.StatusNotFound, err.Error())
			return
		}
	}
	return r.passthroughRequest(sw, in, p, route, &url.URL{Path: rest, RawQuery: query}, model, dialect, dbxGemini)
}

func (r *Router) passthroughRoute(w http.ResponseWriter,
	provider, dialect string) (p *providers.Provider, route string, dbxGemini, ok bool) {
	reg, err := r.registry()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "proxy: "+err.Error())
		return nil, "", false, false
	}
	p, err = reg.Get(provider)
	if err != nil {
		writeError(w, http.StatusBadRequest, "proxy: "+err.Error())
		return nil, "", false, false
	}
	route, ok = p.Route(dialect)
	if !ok && dialect == providers.ProtocolGemini {
		route, ok = databricksGemini(p)
		dbxGemini = ok
	}
	if !ok {
		writeError(w, http.StatusNotFound, "proxy: provider "+p.Name+" has no "+dialect+" route")
		return nil, "", false, false
	}
	return p, route, dbxGemini, true
}

func (r *Router) passthroughRequest(sw *statusWriter, in *http.Request, p *providers.Provider,
	route string, path *url.URL, model, dialect string, dbxGemini bool) (string, string) {
	var body []byte
	geminiChat := false
	if in.Method == http.MethodPost {
		var ok bool
		if body, ok = readBody(sw, in); !ok {
			return model, ""
		}
		model = cmp.Or(passthroughModel(body), model)
		if dbxGemini {
			body = dropOffThoughts(body)
		}
		if isChatCompletion(dialect, path.Path) {
			if translate.IsGeminiModel(model) {
				body = fixGeminiChat(body)
				geminiChat = true
			}
			body = dropNonFunctionTools(body)
		}
		if dialect != providers.ProtocolGemini {
			body = clampOutputTokens(body, model)
		}
	}
	return r.passthroughResponse(sw, in, p, route, path, model, dialect, body, geminiChat)
}

func (r *Router) passthroughResponse(sw *statusWriter, in *http.Request, p *providers.Provider,
	route string, path *url.URL, model, dialect string, body []byte, geminiChat bool) (string, string) {
	target := route + "/" + path.Path
	if path.RawQuery != "" {
		target += "?" + path.RawQuery
	}
	resp, err := r.sendMethod(in, in.Method, p, target, body, copyClientHeaders(in))
	if err != nil {
		writeSendError(sw, p, err)
		return model, ""
	}
	defer func() { _ = resp.Body.Close() }() // whichever reply is current: the retry swaps resp
	retried := ""
	if isChatCompletion(dialect, path.Path) {
		resp, body, retried = r.retryWithoutEffort(in, p, target, model, resp, body)
	}
	extra := passthroughDiag(resp, len(body)) + retried
	if resp.StatusCode == http.StatusTooManyRequests && !providers.ModelDenied(string(peekBody(resp, 64<<10))) {
		r.coolDown(p, resp) // a passthrough 429 sinks p in this proxy's /v1/messages ladders too
	}
	if err := flattenPassthroughResponse(resp, geminiChat); err != nil {
		writeError(sw, http.StatusBadGateway, "proxy: "+p.Name+": "+err.Error())
		return model, extra
	}
	tail := teeUsageTail(resp, in, p, dialect)
	relay(sw, resp)
	if tail != nil {
		logLedgerUsage(in, p, model, scrapeUsage(tail.buf))
	}
	return model, extra
}

func isChatCompletion(dialect, path string) bool {
	return dialect == providers.ProtocolOpenAI && strings.HasSuffix(path, "chat/completions")
}

func flattenPassthroughResponse(resp *http.Response, geminiChat bool) error {
	if geminiChat && resp.StatusCode == http.StatusOK &&
		!strings.Contains(resp.Header.Get("Content-Type"), "event-stream") {
		return flattenGeminiResponse(resp)
	}
	return nil
}

// retryWithoutEffort resends a chat body once with reasoning_effort "none" when the upstream 400s on tools with it.
func (r *Router) retryWithoutEffort(in *http.Request, p *providers.Provider, target, model string,
	resp *http.Response, body []byte) (*http.Response, []byte, string) {
	if resp.StatusCode != http.StatusBadRequest || !bytes.Contains(body, []byte(`"reasoning_effort"`)) {
		return resp, body, ""
	}
	msg := strings.ToLower(string(peekBody(resp, 64<<10)))
	if !strings.Contains(msg, "function tools with reasoning_effort") {
		return resp, body, ""
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return resp, body, ""
	}
	top["reasoning_effort"] = json.RawMessage(`"none"`) // deleting it leaves the model default, refused the same
	stripped, err := json.Marshal(top)
	if err != nil {
		return resp, body, ""
	}
	retry, err := r.sendMethod(in, in.Method, p, target, stripped, copyClientHeaders(in))
	if err != nil {
		return resp, body, ""
	}
	_ = resp.Body.Close()
	fmt.Fprintf(r.log, "proxy: %s: model %s rejected function tools with reasoning_effort; retried with none\n",
		p.Name, agentToken(model))
	return retry, stripped, " reasoning_effort=none"
}

// copyClientHeaders returns a hook copying the client's non-hop, non-blocked headers onto the upstream request.
func copyClientHeaders(in *http.Request) func(http.Header) {
	listed := map[string]bool{} // headers the client's Connection value marks hop-by-hop
	for _, v := range in.Header.Values("Connection") {
		for _, name := range textutil.SplitCSV(v) {
			listed[http.CanonicalHeaderKey(name)] = true
		}
	}
	return func(h http.Header) {
		for k, vs := range in.Header {
			ck := http.CanonicalHeaderKey(k)
			if !hopHeaders[ck] && !noForward[ck] && !listed[ck] {
				h[ck] = append([]string(nil), vs...)
			}
		}
	}
}

// writeSendError answers a failed upstream send: 401 with a hint for auth failures, else 502.
func writeSendError(w http.ResponseWriter, p *providers.Provider, err error) {
	var failure *fallbackError
	if errors.As(err, &failure) && failure.auth {
		writeError(w, http.StatusUnauthorized, fmt.Sprintf("proxy: %s: authentication failed; %s", p.Name, authHint(p)))
		return
	}
	writeError(w, http.StatusBadGateway, "proxy: "+p.Name+": "+err.Error())
}

// teeUsageTail wraps a metered 200 POST reply so its closing usage chunk is captured; nil when not metered.
func teeUsageTail(resp *http.Response, in *http.Request, p *providers.Provider, dialect string) *tailBuffer {
	if !metered(p) || in.Method != http.MethodPost || resp.StatusCode != http.StatusOK ||
		dialect == providers.ProtocolGemini {
		return nil
	}
	tail := &tailBuffer{}
	resp.Body = teeReadCloser{io.TeeReader(resp.Body, tail), resp.Body}
	return tail
}

// flattenGeminiResponse buffers a non-stream Gemini chat reply and joins its content parts into one string.
func flattenGeminiResponse(resp *http.Response) error {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	_ = resp.Body.Close()
	if err != nil {
		return fmt.Errorf("read upstream reply: %w", err)
	}
	if len(raw) > maxBody {
		return fmt.Errorf("upstream reply exceeds %d bytes", maxBody)
	}
	out := translate.FlattenChatContent(raw)
	resp.Body, resp.ContentLength = io.NopCloser(bytes.NewReader(out)), int64(len(out))
	resp.Header.Del("Content-Length")
	return nil
}

// geminiTools strips the schema keys Google rejects from every tool's parameters; false when none changed.
func geminiTools(raw json.RawMessage) (json.RawMessage, bool) {
	var tools []map[string]json.RawMessage
	if json.Unmarshal(raw, &tools) != nil {
		return raw, false
	}
	changed := false
	for _, t := range tools {
		var fn map[string]json.RawMessage
		if json.Unmarshal(t["function"], &fn) != nil {
			continue
		}
		if params, ok := translate.GeminiSchema(fn["parameters"]); ok {
			fn["parameters"] = params
			t["function"], _ = json.Marshal(fn)
			changed = true
		}
	}
	if !changed {
		return raw, false
	}
	out, err := json.Marshal(tools)
	return out, err == nil
}

// dropNonFunctionTools removes tools[] entries without a function object (custom, web_search), which chat endpoints 400.
func dropNonFunctionTools(body []byte) []byte {
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return body
	}
	var tools []map[string]json.RawMessage
	if json.Unmarshal(top["tools"], &tools) != nil {
		return body
	}
	kept := make([]map[string]json.RawMessage, 0, len(tools))
	for _, t := range tools {
		if fn := bytes.TrimSpace(t["function"]); len(fn) > 0 && fn[0] == '{' {
			kept = append(kept, t)
		}
	}
	if len(kept) == len(tools) {
		return body
	}
	if len(kept) == 0 {
		delete(top, "tools")
		delete(top, "tool_choice")
		delete(top, "parallel_tool_calls")
	} else {
		top["tools"], _ = json.Marshal(kept)
	}
	out, err := json.Marshal(top)
	if err != nil {
		return body
	}
	return out
}

// fixGeminiChat normalizes streaming options, tool choice, schemas, and thought signatures.
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
	if tc, ok := top["tool_choice"]; ok {
		if fixed, ok := translate.ChatToolChoice(tc); ok {
			top["tool_choice"], changed = fixed, true
		}
	}
	if fixed, ok := geminiTools(top["tools"]); ok {
		top["tools"], changed = fixed, true
	}
	changed = geminiSignatures(top, changed)
	if !changed {
		return body
	}
	out, err := json.Marshal(top)
	if err != nil {
		return body
	}
	return out
}

// outputTokenKeys are the top-level request keys that set an output cap on the OpenAI chat and Responses wires.
var outputTokenKeys = [...]string{"max_tokens", "max_completion_tokens", "max_output_tokens"}

// clampOutputTokens lowers any output cap above model's catalog limit, so the endpoint can't 400; else body as is.
func clampOutputTokens(body []byte, model string) []byte {
	limits, ok := models.Limits(model)
	if !ok || limits.Output <= 0 {
		return body
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return body
	}
	if !clampCaps(top, limits.Output) {
		return body
	}
	out, err := json.Marshal(top)
	if err != nil {
		return body
	}
	return out
}

// clampCaps lowers every output cap in top above max to max; false when none changed.
func clampCaps(top map[string]json.RawMessage, max int) bool {
	changed := false
	for _, key := range outputTokenKeys {
		var asked float64
		if json.Unmarshal(top[key], &asked) != nil || asked <= float64(max) {
			continue
		}
		top[key], changed = json.RawMessage(strconv.Itoa(max)), true
	}
	return changed
}

// tailBuffer keeps the last tailCap bytes written, so a stream's closing usage chunk survives without buffering it all.
type tailBuffer struct{ buf []byte }

const tailCap = 256 << 10

// headTailBuffer keeps the first and last tailCap bytes written: usage sits in a stream's first and last events.
type headTailBuffer struct {
	head []byte
	tail tailBuffer
}

func (h *headTailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := tailCap - len(h.head); room > 0 {
		h.head = append(h.head, p[:min(room, len(p))]...)
		p = p[min(room, len(p)):]
	}
	_, _ = h.tail.Write(p)
	return n, nil
}

// Bytes returns the head followed by the retained tail, dropping an oversized middle.
func (h *headTailBuffer) Bytes() []byte {
	t := h.tail.buf
	return append(append([]byte(nil), h.head...), t[max(0, len(t)-tailCap):]...)
}

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
	return scrapeLastUsage(body, "input_tokens", "output_tokens", "input_tokens_details")
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

// passthroughDiag is the log suffix: request id, Retry-After, request bytes and, on an error, its code and message.
func passthroughDiag(resp *http.Response, reqBytes int) string {
	var b strings.Builder
	for _, k := range []string{"X-Request-Id", "X-Databricks-Request-Id", "X-Databricks-Requestid", "Request-Id"} {
		if v := resp.Header.Get(k); v != "" {
			b.WriteString(" req=")
			b.WriteString(agentToken(v))
			break
		}
	}
	if v := resp.Header.Get("Retry-After"); v != "" {
		b.WriteString(" retry_after=")
		b.WriteString(agentToken(v))
	}
	if reqBytes > 0 {
		b.WriteString(" bytes=")
		b.WriteString(strconv.Itoa(reqBytes))
	}
	if resp.StatusCode >= 400 {
		b.WriteString(errDiag(peekBody(resp, 64<<10)))
	}
	return b.String()
}

// errDiag is the " err=<code> msg=<message>" log suffix read from an upstream error body; "" when it names neither.
func errDiag(body []byte) string {
	var e struct {
		Code    string `json:"error_code"`
		Message string `json:"message"`
		Error   struct {
			Code    string `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	out := ""
	if c := cmp.Or(e.Code, e.Error.Code, e.Error.Type); c != "" {
		out += " err=" + agentToken(c)
	}
	if m := logMessage(cmp.Or(e.Message, e.Error.Message)); m != "" {
		out += " msg=" + m
	}
	return out
}

// logMessage is an upstream error message on one line: control characters dropped, capped at 160 runes.
func logMessage(s string) string {
	var b strings.Builder
	n := 0
	for _, c := range s {
		if n == 160 {
			break
		}
		if !unicode.IsControl(c) {
			b.WriteRune(c)
			n++
		}
	}
	return strings.TrimSpace(b.String())
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
		return "", "", "", errors.New(
			"proxy: databricks gemini route serves only v1beta/models/<id>:(stream)GenerateContent",
		)
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
	if _, ok := think["includeThoughts"]; !ok || json.Unmarshal(think["thinkingBudget"], &budget) != nil ||
		budget != 0 {
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

func geminiSignatures(top map[string]json.RawMessage, changed bool) bool {
	var msgs []map[string]json.RawMessage
	if json.Unmarshal(top["messages"], &msgs) != nil {
		return changed
	}
	for _, m := range msgs {
		var calls []map[string]json.RawMessage
		if json.Unmarshal(m["tool_calls"], &calls) != nil || !signCalls(calls) {
			continue
		}
		m["tool_calls"], _ = json.Marshal(calls)
		changed = true
	}
	if changed {
		top["messages"], _ = json.Marshal(msgs)
	}
	return changed
}

// signCalls adds the skip thought signature to each call lacking one; true when any changed.
func signCalls(calls []map[string]json.RawMessage) bool {
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
	return touched
}
