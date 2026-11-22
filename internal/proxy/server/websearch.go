package server

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy/translate"
	"github.com/edotau/claude-code/internal/serving"
	"github.com/edotau/claude-code/internal/websearch"
)

// searchRoundCap bounds one request's web searches, and the rounds a model may spend after its max_uses.
const searchRoundCap = 10

// searchResults is how many hits one search returns to the model.
const searchResults = 5

// searchWire is one upstream wire the search loop runs over: a round trip and the next body after tool results.
type searchWire interface {
	round(w *statusWriter, in *http.Request, body []byte) (reply *translate.Reply, done bool, err error)
	next(body []byte, reply *translate.Reply, results []translate.ToolResult) ([]byte, error)
}

// chatWire is the OpenAI chat route of a translated request.
type chatWire struct {
	r    *Router
	t    providers.Target
	last bool
}

func (c chatWire) round(w *statusWriter, in *http.Request, body []byte) (*translate.Reply, bool, error) {
	return c.r.chatRound(w, in, c.t, body, c.last)
}

func (chatWire) next(body []byte, reply *translate.Reply, results []translate.ToolResult) ([]byte, error) {
	return translate.AppendRound(body, reply, results)
}

// tryWebSearch answers a request carrying the web_search server tool: it runs the model's searches itself and loops.
func (r *Router) tryWebSearch(w *statusWriter, in *http.Request, chat []byte, ws *translate.WebSearch, t providers.Target,
	clientModel string, stream, last bool) (bool, error) {
	chat, err := translate.NonStreaming(chat)
	if err != nil {
		return false, err
	}
	return r.searchLoop(w, in, chat, searchJob{chatWire{r, t, last}, ws, t, clientModel, stream})
}

// searchJob is what a search loop answers for: the tool's limits, the target and the client's model and stream flag.
type searchJob struct {
	wire        searchWire
	ws          *translate.WebSearch
	t           providers.Target
	clientModel string
	stream      bool
}

func (r *Router) searchLoop(w *statusWriter, in *http.Request, body []byte, job searchJob) (bool, error) {
	ws, t, stream, wire := job.ws, job.t, job.stream, job.wire
	limit := searchLimit(ws)
	ans := &translate.Answer{Model: reportedModel(job.clientModel, t)}
	for round := 0; round <= limit+2 && ans.Stop == ""; round++ {
		reply, done, err := wire.round(w, in, body)
		if done || err != nil {
			return done, err
		}
		if body, err = r.searchStep(in, job, ans, reply, body); err != nil {
			return false, err
		}
	}
	if ans.Stop == "" {
		ans.End("stop")
	}
	if err := writeAnswer(w, ans, stream); err != nil {
		fmt.Fprintf(r.log, "proxy: %s web_search reply: %v\n", t.Provider.Name, err)
	}
	return true, nil
}

// searchOverChat reports a Databricks request whose web_search server tool its Anthropic route rejects.
func searchOverChat(p *providers.Provider, body []byte) bool {
	_, chat := p.Route(providers.ProtocolOpenAI)
	return chat && p.Preset == providers.PresetDatabricks && translate.WebSearchOf(body) != nil
}

// anthropicWire runs the search loop on a Databricks Anthropic route, which rejects the server tool.
type anthropicWire struct {
	r     *Router
	t     providers.Target
	route string
	last  bool
	reply json.RawMessage // the last reply's content blocks, replayed as the assistant turn
}

func (a *anthropicWire) round(w *statusWriter, in *http.Request, body []byte) (*translate.Reply, bool, error) {
	p := a.t.Provider
	url := serving.EndpointURL(a.route, providers.ProtocolAnthropic, a.t.Model, false)
	headers := func(h http.Header) {
		copyRequestHeaders(h, in.Header)
		dropDatabricksBetas(p, h)
		h.Set("Content-Type", "application/json")
		h.Set("Accept", "application/json")
	}
	resp, err := a.r.sendWithAuth(in, p, func(credential string) (*http.Response, error) {
		return a.r.sendRequestBody(in, http.MethodPost, p, url, proxyBody{raw: body, model: a.t.Model}, headers, credential)
	})
	if err != nil {
		return nil, false, err
	}
	if moveOn, hard := a.r.failover(resp, a.t, a.last); moveOn {
		return nil, false, &fallbackError{error: errors.New(resp.Status), status: resp.StatusCode, hard: hard}
	}
	if resp.StatusCode != http.StatusOK {
		relay(w, resp)
		return nil, true, nil
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, false, err
	}
	if len(b) > maxBody {
		return nil, false, fmt.Errorf("upstream reply exceeds %d bytes", maxBody)
	}
	if metered(p) {
		logLedgerUsage(in, p, a.t.Model, scrapeAnthropicUsage(b))
	}
	chat, content, err := anthropicAsChat(b)
	if err != nil {
		return nil, false, err
	}
	a.reply = content
	reply, err := translate.ParseReply(chat)
	return reply, false, err
}

func (a *anthropicWire) next(body []byte, _ *translate.Reply, results []translate.ToolResult) ([]byte, error) {
	var blocks []map[string]any
	for _, res := range results {
		blocks = append(blocks, map[string]any{"type": "tool_result", "tool_use_id": res.ID, "content": res.Text})
	}
	user, err := json.Marshal(blocks)
	if err != nil {
		return nil, err
	}
	return appendMessages(body, []map[string]any{{"role": "assistant", "content": a.reply}, {"role": "user", "content": json.RawMessage(user)}})
}

// anthropicSearchJob is the loop for a Databricks request with web_search and no chat route to divert it to.
func (r *Router) anthropicSearchJob(t providers.Target, body []byte, route, clientModel string, last bool) (searchJob, bool) {
	if route == "" || t.Provider.Preset != providers.PresetDatabricks || searchOverChat(t.Provider, body) {
		return searchJob{}, false
	}
	ws := translate.WebSearchOf(body)
	return searchJob{wire: &anthropicWire{r: r, t: t, route: route, last: last}, ws: ws, t: t, clientModel: clientModel}, ws != nil
}

// tryAnthropicSearch answers web_search on a Databricks Anthropic route: the server tool becomes a client tool.
func (r *Router) tryAnthropicSearch(w *statusWriter, in *http.Request, body []byte, job searchJob) (bool, error) {
	out, stream, err := anthropicSearchBody(body, job.t.Model)
	if err != nil {
		return false, err
	}
	job.stream = stream
	return r.searchLoop(w, in, out, job)
}

// anthropicSearchBody makes a Messages body one buffered reply asking for web_search as a client tool.
func anthropicSearchBody(body []byte, model string) (out []byte, stream bool, err error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, false, err
	}
	_ = json.Unmarshal(top["stream"], &stream)
	delete(top, "stream")
	top["model"], _ = json.Marshal(model)
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(top["tools"], &tools); err != nil {
		return nil, false, err
	}
	for i, t := range tools {
		var name, typ string
		_ = json.Unmarshal(t["name"], &name)
		_ = json.Unmarshal(t["type"], &typ)
		if name == "web_search" && strings.HasPrefix(typ, "web_search") {
			tools[i] = map[string]json.RawMessage{"name": json.RawMessage(`"web_search"`),
				"description":  json.RawMessage(`"Search the web for current information; returns result titles, urls and snippets."`),
				"input_schema": json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"the search query"}},"required":["query"]}`)}
		}
	}
	top["tools"], _ = json.Marshal(tools)
	if top["messages"], err = flattenSearchHistory(top["messages"]); err != nil {
		return nil, false, err
	}
	out, err = json.Marshal(top)
	return out, stream, err
}

// flattenSearchHistory turns earlier turns' server search blocks into plain text, which the route accepts.
func flattenSearchHistory(raw json.RawMessage) (json.RawMessage, error) {
	var msgs []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return nil, err
	}
	for _, m := range msgs {
		var blocks []map[string]any
		if json.Unmarshal(m["content"], &blocks) != nil {
			continue
		}
		kept := blocks[:0]
		for _, b := range blocks {
			switch b["type"] {
			case "server_tool_use":
				continue
			case "web_search_tool_result":
				b = map[string]any{"type": "text", "text": "Web search results:\n" + searchBlockText(b["content"])}
			}
			kept = append(kept, b)
		}
		m["content"], _ = json.Marshal(kept)
	}
	return json.Marshal(msgs)
}

func searchBlockText(content any) string {
	list, _ := content.([]any)
	var hits []translate.SearchHit
	for _, e := range list {
		m, _ := e.(map[string]any)
		title, _ := m["title"].(string)
		url, _ := m["url"].(string)
		snippet, _ := m["encrypted_content"].(string)
		hits = append(hits, translate.SearchHit{Title: title, URL: url, Snippet: snippet})
	}
	return hitsText(hits)
}

// appendMessages adds turns to a body's messages.
func appendMessages(body []byte, turns []map[string]any) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, err
	}
	var msgs []any
	if err := json.Unmarshal(top["messages"], &msgs); err != nil {
		return nil, err
	}
	for _, t := range turns {
		msgs = append(msgs, t)
	}
	top["messages"], _ = json.Marshal(msgs)
	return json.Marshal(top)
}

// anthropicAsChat re-expresses a Messages reply as a chat-completions one, so the search loop reads both wires alike.
func anthropicAsChat(body []byte) (chat []byte, content json.RawMessage, err error) {
	var raw struct {
		Content []struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Text  string          `json:"text"`
			Input json.RawMessage `json:"input"`
		} `json:"-"`
		Blocks     json.RawMessage `json:"content"`
		StopReason string          `json:"stop_reason"`
		Usage      struct {
			In        int `json:"input_tokens"`
			Out       int `json:"output_tokens"`
			CacheRead int `json:"cache_read_input_tokens"`
			CacheWrt  int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, nil, fmt.Errorf("anthropic reply: %w", err)
	}
	if err := json.Unmarshal(raw.Blocks, &raw.Content); err != nil {
		return nil, nil, fmt.Errorf("anthropic reply content: %w", err)
	}
	var text strings.Builder
	var calls []map[string]any
	for _, b := range raw.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			calls = append(calls, map[string]any{"id": b.ID, "type": "function",
				"function": map[string]any{"name": b.Name, "arguments": string(b.Input)}})
		}
	}
	msg := map[string]any{"role": "assistant", "content": text.String()}
	if len(calls) > 0 {
		msg["tool_calls"] = calls
	}
	finish := cmp.Or(map[string]string{"max_tokens": "length", "tool_use": "tool_calls"}[raw.StopReason], "stop")
	u := raw.Usage
	chat, err = json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": msg, "finish_reason": finish}},
		"usage": map[string]any{"prompt_tokens": u.In + u.CacheRead + u.CacheWrt, "completion_tokens": u.Out,
			"prompt_tokens_details": map[string]any{"cached_tokens": u.CacheRead}}})
	return chat, raw.Blocks, err
}

// searchLimit is the request's max_uses, held to searchRoundCap.
func searchLimit(ws *translate.WebSearch) int {
	if ws.MaxUses <= 0 || ws.MaxUses > searchRoundCap {
		return searchRoundCap
	}
	return ws.MaxUses
}

// searchStep records one upstream reply on ans; when the model asked only for searches it returns the next chat body.
func (r *Router) searchStep(in *http.Request, job searchJob, ans *translate.Answer,
	reply *translate.Reply, chat []byte) ([]byte, error) {
	ws, wire := job.ws, job.wire
	limit := searchLimit(ws)
	ans.AddUsage(reply.Usage)
	ans.AddText(reply.Text)
	searches, others := splitSearches(reply.Calls)
	results := r.runSearches(in, ws, limit, ans, searches)
	if len(searches) > 0 && len(others) == 0 {
		return wire.next(chat, reply, results)
	}
	ans.AddCalls(others)
	ans.End(reply.Finish)
	return chat, nil
}

// splitSearches separates the web_search calls the gateway answers from the client's own tool calls.
func splitSearches(calls []translate.Call) (searches, others []translate.Call) {
	for _, c := range calls {
		if c.Name == "web_search" {
			searches = append(searches, c)
		} else {
			others = append(others, c)
		}
	}
	return searches, others
}

// runSearches searches for each call, records the blocks on ans, and returns the tool results for the model.
func (r *Router) runSearches(in *http.Request, ws *translate.WebSearch, limit int, ans *translate.Answer,
	calls []translate.Call) []translate.ToolResult {
	var out []translate.ToolResult
	for _, c := range calls {
		q := strings.TrimSpace(c.SearchQuery())
		var hits []translate.SearchHit
		code, text := "", ""
		switch {
		case q == "":
			code, text = translate.SearchBadInput, "web_search needs a non-empty query"
		case ans.Searches >= limit:
			code, text = translate.SearchMaxUses, "the web search limit is reached; answer with what you have"
		default:
			found, err := websearch.Search(in.Context(), q, searchResults, ws.Allowed, ws.Blocked)
			if err != nil {
				fmt.Fprintf(r.log, "proxy: web_search: %v\n", err)
				code, text = translate.SearchUnavailable, "web search is unavailable right now"
				break
			}
			for _, h := range found {
				hits = append(hits, translate.SearchHit{Title: h.Title, URL: h.URL, Snippet: h.Snippet})
			}
			text = hitsText(hits)
		}
		ans.AddSearch(q, hits, code)
		out = append(out, translate.ToolResult{ID: c.ID, Text: text})
	}
	return out
}

// hitsText renders hits as the tool result text the model reads.
func hitsText(hits []translate.SearchHit) string {
	if len(hits) == 0 {
		return "No results found."
	}
	var b strings.Builder
	for _, h := range hits {
		fmt.Fprintf(&b, "%s\n%s\n%s\n\n", h.Title, h.URL, h.Snippet)
	}
	return strings.TrimSpace(b.String())
}

// chatRound sends one buffered chat request; done means the reply was already written (an upstream error status).
func (r *Router) chatRound(w *statusWriter, in *http.Request, t providers.Target, chat []byte, last bool) (
	reply *translate.Reply, done bool, err error) {
	p := t.Provider
	route, _ := p.Route(providers.ProtocolOpenAI)
	resp, err := r.send(in, p, route+"/chat/completions", chat, func(h http.Header) {
		h.Set("Content-Type", "application/json")
	})
	if err != nil {
		return nil, false, err
	}
	if moveOn, hard := r.failover(resp, t, last); moveOn {
		return nil, false, &fallbackError{error: errors.New(resp.Status), status: resp.StatusCode, hard: hard}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, false, err
	}
	if resp.StatusCode != http.StatusOK {
		writeJSON(w, resp.StatusCode, translate.FromChatError(resp.StatusCode, b))
		return nil, true, nil
	}
	if len(b) > maxBody {
		return nil, false, fmt.Errorf("upstream reply exceeds %d bytes", maxBody)
	}
	if metered(p) {
		logLedgerUsage(in, p, t.Model, scrapeChatUsage(b))
	}
	reply, err = translate.ParseReply(b)
	return reply, false, err
}

// writeAnswer sends ans as one JSON reply, or as an event stream when the client asked for one.
func writeAnswer(w *statusWriter, ans *translate.Answer, stream bool) error {
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		return ans.WriteSSE(w, flusher(w))
	}
	body, err := ans.JSON()
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, body)
	return nil
}
