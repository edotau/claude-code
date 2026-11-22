package hooks

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/websearch"
)

const (
	webTavilyTimeout = 20 * time.Second
	webAnswerPrefix  = "Question asked: "
	tavilyKeyName    = "TAVILY_API_KEY"
)

// Telemetry verdicts: which path answered the fetch or search.
const (
	webReplaced  = "replaced"
	webSkipMod   = "skipped-mod"
	webSkipProxy = "skipped-proxy"
	webFallback  = "fallback"
)

var sessionOnDatabricks = func() bool {
	p := providers.SessionProvider()
	return p != nil && p.Preset == providers.PresetDatabricks
}

// WebTavily (PostToolUse WebFetch|WebSearch) swaps the built-in result for Tavily's; any failure keeps the original.
func WebTavily(in *core.Input) core.Result {
	if src, err := vault.HasSecretErr(tavilyKeyName); err == nil && src == "" {
		return core.Result{} // no key anywhere: nothing to try
	}
	resp, _ := in.Field("tool_response").(map[string]any)
	ctx, cancel := context.WithTimeout(context.Background(), webTavilyTimeout)
	defer cancel()
	switch in.ToolName {
	case "WebFetch":
		return webFetchTavily(ctx, in.ToolInput(), resp)
	case "WebSearch":
		return webSearchTavily(ctx, in.ToolInput(), resp)
	}
	return core.Result{}
}

func webFetchTavily(ctx context.Context, args, resp map[string]any) core.Result {
	url, _ := args["url"].(string)
	if url == "" {
		return core.Result{}
	}
	if s, _ := resp["result"].(string); strings.HasPrefix(s, webAnswerPrefix) {
		return core.Result{Verdict: webSkipMod} // the tavily-web mod already answered
	}
	text, ok, err := websearch.Extract(ctx, []string{url})
	if err != nil || ok == 0 {
		return core.Result{Verdict: webFallback}
	}
	if off, _ := args["offset"].(float64); off > 0 {
		r := []rune(text)
		text = string(r[min(int(off), len(r)):])
	}
	prompt, _ := args["prompt"].(string)
	out := map[string]any{}
	if resp != nil {
		out = maps.Clone(resp)
	}
	out["result"] = webAnswerPrefix + prompt + "\n\n" + strings.TrimSpace(text)
	return core.Result{UpdatedToolOutput: out, Verdict: webReplaced}
}

// webSearchTavily answers in WebSearch's shape: {query, results: [{tool_use_id, content: [{title, url}]}, text]}.
func webSearchTavily(ctx context.Context, args, resp map[string]any) core.Result {
	query, _ := args["query"].(string)
	if query == "" {
		return core.Result{}
	}
	if sessionOnDatabricks() {
		return core.Result{Verdict: webSkipProxy} // the proxy already searched through Tavily
	}
	start := time.Now()
	hits, err := websearch.Search(ctx, query, websearch.DefaultMaxResults, stringList(args["allowed_domains"]),
		stringList(args["blocked_domains"]))
	if err != nil || len(hits) == 0 {
		return core.Result{Verdict: webFallback}
	}
	links := make([]any, 0, len(hits))
	var sb strings.Builder
	fmt.Fprintf(&sb, "Web search results for query: %q (Tavily)\n", query)
	for _, h := range hits {
		links = append(links, map[string]any{"title": h.Title, "url": h.URL})
		fmt.Fprintf(&sb, "\n- [%s](%s): %s", h.Title, h.URL, strings.Join(strings.Fields(h.Snippet), " "))
	}
	out := map[string]any{}
	if resp != nil {
		out = maps.Clone(resp)
	}
	out["query"] = query
	out["results"] = []any{map[string]any{"tool_use_id": searchToolUseID(resp), "content": links}, sb.String()}
	out["durationSeconds"] = time.Since(start).Seconds()
	return core.Result{UpdatedToolOutput: out, Verdict: webReplaced}
}

// searchToolUseID keeps the built-in result's server tool id, so the replacement points at the same search.
func searchToolUseID(resp map[string]any) string {
	results, _ := resp["results"].([]any)
	for _, r := range results {
		if m, ok := r.(map[string]any); ok {
			if id, _ := m["tool_use_id"].(string); id != "" {
				return id
			}
		}
	}
	return "tavily"
}

func stringList(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s, _ := it.(string); s != "" {
			out = append(out, s)
		}
	}
	return out
}
