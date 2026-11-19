package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/edotau/claude-code/internal/gateway/translate"
	"github.com/edotau/claude-code/internal/network/websearch"
	"github.com/edotau/claude-code/internal/providers"
)

// searchRoundCap bounds one request's web searches, and the rounds a model may spend after its max_uses.
const searchRoundCap = 10

// searchResults is how many hits one search returns to the model.
const searchResults = 5

// tryWebSearch answers a request carrying the web_search server tool: it runs the model's searches itself and loops.
func (r *Router) tryWebSearch(w *statusWriter, in *http.Request, chat []byte, ws *translate.WebSearch, t providers.Target,
	clientModel string, stream, last bool) (bool, error) {
	chat, err := translate.NonStreaming(chat)
	if err != nil {
		return false, err
	}
	limit := searchLimit(ws)
	ans := &translate.Answer{Model: reportedModel(clientModel, t)}
	for round := 0; round <= limit+2 && ans.Stop == ""; round++ {
		reply, done, err := r.chatRound(w, in, t, chat, last)
		if done || err != nil {
			return done, err
		}
		if chat, err = r.searchStep(in, ws, limit, ans, reply, chat); err != nil {
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

// searchLimit is the request's max_uses, held to searchRoundCap.
func searchLimit(ws *translate.WebSearch) int {
	if ws.MaxUses <= 0 || ws.MaxUses > searchRoundCap {
		return searchRoundCap
	}
	return ws.MaxUses
}

// searchStep records one upstream reply on ans; when the model asked only for searches it returns the next chat body.
func (r *Router) searchStep(in *http.Request, ws *translate.WebSearch, limit int, ans *translate.Answer,
	reply *translate.Reply, chat []byte) ([]byte, error) {
	ans.AddUsage(reply.Usage)
	ans.AddText(reply.Text)
	searches, others := splitSearches(reply.Calls)
	results := r.runSearches(in, ws, limit, ans, searches)
	if len(searches) > 0 && len(others) == 0 {
		return translate.AppendRound(chat, reply, results)
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
