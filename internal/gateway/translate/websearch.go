package translate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const webSearchName = "web_search"

// Error codes of a web_search_tool_result_error block.
const (
	SearchUnavailable = "unavailable"
	SearchMaxUses     = "max_uses_exceeded"
	SearchBadInput    = "invalid_tool_input"
)

// WebSearch is a request's web_search server tool: the limits it asks for.
type WebSearch struct {
	MaxUses int
	Allowed []string
	Blocked []string
}

func isWebSearch(t inTool) bool {
	return t.Name == webSearchName && strings.HasPrefix(t.Type, "web_search")
}

// WebSearchOf returns the web_search server tool in an Anthropic request body, or nil.
func WebSearchOf(body []byte) *WebSearch {
	var in struct {
		Tools []inTool `json:"tools"`
	}
	if json.Unmarshal(body, &in) != nil {
		return nil
	}
	for _, t := range in.Tools {
		if isWebSearch(t) {
			return &WebSearch{MaxUses: t.MaxUses, Allowed: t.AllowedDomains, Blocked: t.BlockedDomains}
		}
	}
	return nil
}

// webSearchFunction is the function tool a chat upstream sees in place of the server tool.
func webSearchFunction() chatTool {
	return chatTool{Type: "function", Function: toolDef{
		Name:        webSearchName,
		Description: "Search the web for current information; returns result titles, urls and snippets.",
		Parameters: json.RawMessage(
			`{"type":"object","properties":{"query":{"type":"string","description":"the search query"}},"required":["query"]}`),
	}}
}

// Call is one tool call in a chat reply.
type Call struct{ ID, Name, Args string }

// Reply is one non-streaming chat reply, reduced to what the search loop reads.
type Reply struct {
	Text   string
	Calls  []Call
	Finish string
	Usage  *chatUsage
}

// ParseReply reads a non-streaming chat-completions body.
func ParseReply(body []byte) (*Reply, error) {
	var in chatResponse
	if err := json.Unmarshal(FlattenChatContent(body), &in); err != nil {
		return nil, fmt.Errorf("translate: response: %w", err)
	}
	if len(in.Error) > 0 && string(in.Error) != "null" {
		return nil, fmt.Errorf("translate: upstream error: %s", in.Error)
	}
	if len(in.Choices) == 0 {
		return nil, errors.New("translate: response has no choices")
	}
	c := in.Choices[0]
	text, err := contentText(c.Message.Content)
	if err != nil {
		return nil, err
	}
	r := &Reply{Text: text, Finish: c.FinishReason, Usage: in.Usage}
	for _, tc := range c.Message.ToolCalls {
		id := tc.ID
		if id == "" {
			id = newID("call_")
		}
		r.Calls = append(r.Calls, Call{ID: id, Name: tc.Function.Name, Args: argsText(tc.Function.Arguments)})
	}
	return r, nil
}

// SearchQuery is the query a web_search call carries.
func (c Call) SearchQuery() string {
	var a struct {
		Query string `json:"query"`
	}
	_ = json.Unmarshal([]byte(c.Args), &a)
	return a.Query
}

// ToolResult answers one tool call by id.
type ToolResult struct{ ID, Text string }

// NonStreaming returns a chat body asking for one buffered reply, without the user field Claude chat endpoints reject.
func NonStreaming(chat []byte) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(chat, &top); err != nil {
		return nil, err
	}
	delete(top, "stream")
	delete(top, "stream_options")
	delete(top, "user")
	return json.Marshal(top)
}

// AppendRound adds the assistant turn r and its tool results to a chat body.
func AppendRound(chat []byte, r *Reply, results []ToolResult) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(chat, &top); err != nil {
		return nil, err
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(top["messages"], &msgs); err != nil {
		return nil, err
	}
	turn := chatMessage{Role: "assistant"}
	if r.Text != "" {
		turn.Content = r.Text
	}
	for _, c := range r.Calls {
		args := c.Args
		if args == "" {
			args = "{}"
		}
		turn.ToolCalls = append(turn.ToolCalls, chatToolCall{ID: c.ID, Type: "function",
			Function: chatFunction{Name: c.Name, Arguments: args}})
	}
	add := []chatMessage{turn}
	for _, res := range results {
		add = append(add, chatMessage{Role: "tool", ToolCallID: res.ID, Content: res.Text})
	}
	for _, m := range add {
		raw, err := json.Marshal(m)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, raw)
	}
	top["messages"], _ = json.Marshal(msgs)
	return json.Marshal(top)
}

// SearchHit is one result the gateway puts in a web_search_tool_result block.
type SearchHit struct{ Title, URL, Snippet string }

// Answer is an Anthropic reply assembled across search rounds.
type Answer struct {
	Model    string
	Content  []map[string]any
	Stop     string
	Searches int
	in, out  int
	cached   int
}

// AddUsage sums one upstream reply's token counts.
func (a *Answer) AddUsage(u *chatUsage) {
	if u == nil {
		return
	}
	cached := 0
	if u.PromptTokensDetails != nil {
		cached = u.PromptTokensDetails.CachedTokens
	}
	in, cr := SplitCached(u.PromptTokens, cached)
	a.in, a.cached, a.out = a.in+in, a.cached+cr, a.out+u.CompletionTokens
}

// AddText appends a text block; empty text adds none.
func (a *Answer) AddText(text string) {
	if text != "" {
		a.Content = append(a.Content, map[string]any{"type": "text", "text": text})
	}
}

// AddCalls appends client tool_use blocks for calls the gateway does not run.
func (a *Answer) AddCalls(calls []Call) {
	for _, c := range calls {
		a.Content = append(a.Content, map[string]any{"type": "tool_use", "id": c.ID, "name": c.Name,
			"input": toolInput(c.Args)})
	}
}

// AddSearch appends the server_tool_use block and its result; a non-empty errCode makes the result an error block.
func (a *Answer) AddSearch(query string, hits []SearchHit, errCode string) {
	id := newID("srvtoolu_")
	if errCode != SearchMaxUses {
		a.Searches++
	}
	result := []map[string]any{}
	for _, h := range hits {
		result = append(result, map[string]any{"type": "web_search_result", "url": h.URL, "title": h.Title,
			"encrypted_content": h.Snippet})
	}
	var content any = result
	if errCode != "" {
		content = map[string]any{"type": "web_search_tool_result_error", "error_code": errCode}
	}
	a.Content = append(a.Content,
		map[string]any{"type": "server_tool_use", "id": id, "name": webSearchName, "input": map[string]any{"query": query}},
		map[string]any{"type": "web_search_tool_result", "tool_use_id": id, "content": content})
}

// End sets the stop reason from the upstream finish_reason and the client tool calls collected.
func (a *Answer) End(finish string) {
	used := false
	for _, b := range a.Content {
		used = used || b["type"] == "tool_use"
	}
	a.Stop = stopReason(finish, used)
}

func (a *Answer) usage() map[string]any {
	u := map[string]any{"input_tokens": a.in, "output_tokens": a.out,
		"server_tool_use": map[string]any{"web_search_requests": a.Searches}}
	if a.cached > 0 {
		u["cache_read_input_tokens"] = a.cached
	}
	return u
}

// JSON renders the reply as an Anthropic Messages response.
func (a *Answer) JSON() ([]byte, error) {
	return json.Marshal(map[string]any{"id": newID("msg_"), "type": "message", "role": "assistant", "model": a.Model,
		"content": a.Content, "stop_reason": a.Stop, "stop_sequence": nil, "usage": a.usage()})
}

// WriteSSE replays the reply as an Anthropic event stream.
func (a *Answer) WriteSSE(w io.Writer, flush func()) error {
	send := func(typ string, payload any) error {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, raw); err == nil && flush != nil {
			flush()
		}
		return err
	}
	start := map[string]any{"id": newID("msg_"), "type": "message", "role": "assistant", "model": a.Model,
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": a.in, "output_tokens": 0}}
	if err := send("message_start", map[string]any{"type": "message_start", "message": start}); err != nil {
		return err
	}
	for i, b := range a.Content {
		if err := sendBlock(send, i, b); err != nil {
			return err
		}
	}
	delta := map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": a.Stop, "stop_sequence": nil},
		"usage": a.usage()}
	if err := send("message_delta", delta); err != nil {
		return err
	}
	return send("message_stop", map[string]any{"type": "message_stop"})
}

// sendBlock streams one content block: its start, the body as a single delta where it has one, and its stop.
func sendBlock(send func(string, any) error, i int, b map[string]any) error {
	opening, delta := b, map[string]any(nil)
	switch b["type"] {
	case "text":
		opening = map[string]any{"type": "text", "text": ""}
		delta = map[string]any{"type": "text_delta", "text": b["text"]}
	case "tool_use", "server_tool_use":
		opening = map[string]any{"type": b["type"], "id": b["id"], "name": b["name"], "input": map[string]any{}}
		raw, _ := json.Marshal(b["input"])
		delta = map[string]any{"type": "input_json_delta", "partial_json": string(raw)}
	}
	if err := send("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": opening}); err != nil {
		return err
	}
	if delta != nil {
		if err := send("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": delta}); err != nil {
			return err
		}
	}
	return send("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
}
