package googletools

import (
	"fmt"
	"slices"
	"strings"
)

// Tool is one Gemini built-in tool the harness can turn on by name.
type Tool string

const (
	Search   Tool = "search"
	URL      Tool = "url"
	Code     Tool = "code"
	Computer Tool = "computer" // held: needs a guard path and the owner's R13 yes
	File     Tool = "file"     // held: needs a corpus id
)

// Header carries a gemini dispatch's tools from agy to the proxy; never forwarded upstream.
const Header = "X-Claude-Code-Google-Tools"

// ModelPrefix marks the header token that names the model the tools are for (agy's title tier gets none).
const ModelPrefix = "model="

// All lists the tools --google-tools accepts, in help order.
var All = []Tool{Search, URL, Code}

var wireKey = map[Tool]string{
	Search: "googleSearch", URL: "urlContext", Code: "codeExecution",
}

// Parse reads names into known tools, deduplicated in input order.
func Parse(names []string) ([]Tool, error) {
	var out []Tool
	for _, n := range names {
		t := Tool(strings.TrimSpace(n))
		switch {
		case t == "":
		case t == Computer || t == File:
			return nil, fmt.Errorf("google-tools: %s is held, not built yet", t)
		case !slices.Contains(All, t):
			return nil, fmt.Errorf("google-tools: unknown %q; one of %s", t, Join(All))
		case !slices.Contains(out, t):
			out = append(out, t)
		}
	}
	return out, nil
}

// Join is tools as a comma list, the header and option spelling.
func Join(tools []Tool) string {
	s := make([]string, len(tools))
	for i, t := range tools {
		s[i] = string(t)
	}
	return strings.Join(s, ",")
}

// Unserved is the asked tools missing from served, in asked order.
func Unserved(served, asked []Tool) []Tool {
	return slices.DeleteFunc(slices.Clone(asked), func(t Tool) bool { return slices.Contains(served, t) })
}

// WireKey is t's generateContent tools[] key.
func WireKey(t Tool) string { return wireKey[t] }
