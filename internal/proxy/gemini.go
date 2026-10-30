package proxy

import (
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"

	"github.com/edotau/claude-code/internal/providers"
)

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
