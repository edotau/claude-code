package hooks

import (
	"context"
	"maps"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/websearch"
)

const (
	webTavilyTimeout = 20 * time.Second
	webAnswerPrefix  = "Question asked: "
)

// Telemetry verdicts: which path answered the fetch.
const (
	webReplaced = "replaced"
	webSkipMod  = "skipped-mod"
	webFallback = "fallback"
)

// WebTavily (PostToolUse WebFetch) swaps a Databricks fetch for a Tavily extract; any failure keeps the original.
func WebTavily(in *core.Input) core.Result {
	args := in.ToolInput()
	url, _ := args["url"].(string)
	if p := providers.SessionProvider(); url == "" || p == nil || p.Preset != providers.PresetDatabricks {
		return core.Result{}
	}
	resp, _ := in.Field("tool_response").(map[string]any)
	if s, _ := resp["result"].(string); strings.HasPrefix(s, webAnswerPrefix) {
		return core.Result{Verdict: webSkipMod} // the tavily-web mod already answered
	}
	ctx, cancel := context.WithTimeout(context.Background(), webTavilyTimeout)
	defer cancel()
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
