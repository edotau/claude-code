package translate

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// GoogleTool is one Gemini built-in tool the harness can turn on by name.
type GoogleTool string

const (
	GoogleSearch   GoogleTool = "search"
	GoogleURL      GoogleTool = "url"
	GoogleCode     GoogleTool = "code"
	GoogleComputer GoogleTool = "computer" // held: needs a guard path and the owner's R13 yes
	GoogleFile     GoogleTool = "file"     // held: needs a corpus id
)

// GoogleToolsHeader carries a gemini dispatch's tools from agy to the proxy; never forwarded upstream.
const GoogleToolsHeader = "X-Claude-Code-Google-Tools"

// GoogleTools lists the tools --google-tools accepts, in help order.
var GoogleTools = []GoogleTool{GoogleSearch, GoogleURL, GoogleCode}

var googleWireKey = map[GoogleTool]string{
	GoogleSearch: "googleSearch", GoogleURL: "urlContext", GoogleCode: "codeExecution",
}

// ParseGoogleTools reads names into known tools, deduplicated in input order.
func ParseGoogleTools(names []string) ([]GoogleTool, error) {
	var out []GoogleTool
	for _, n := range names {
		t := GoogleTool(strings.TrimSpace(n))
		switch {
		case t == "":
		case t == GoogleComputer || t == GoogleFile:
			return nil, fmt.Errorf("google-tools: %s is held, not built yet", t)
		case !slices.Contains(GoogleTools, t):
			return nil, fmt.Errorf("google-tools: unknown %q; one of %s", t, JoinGoogleTools(GoogleTools))
		case !slices.Contains(out, t):
			out = append(out, t)
		}
	}
	return out, nil
}

// JoinGoogleTools is tools as a comma list, the header and option spelling.
func JoinGoogleTools(tools []GoogleTool) string {
	s := make([]string, len(tools))
	for i, t := range tools {
		s[i] = string(t)
	}
	return strings.Join(s, ",")
}

// Unserved is the asked tools missing from served, in asked order.
func Unserved(served, asked []GoogleTool) []GoogleTool {
	return slices.DeleteFunc(slices.Clone(asked), func(t GoogleTool) bool { return slices.Contains(served, t) })
}

// GoogleToolEntries is one generateContent tools[] object per tool, e.g. {"urlContext":{}}.
func GoogleToolEntries(tools []GoogleTool) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(tools))
	for _, t := range tools {
		out = append(out, json.RawMessage(`{"`+googleWireKey[t]+`":{}}`))
	}
	return out
}

// GoogleWireKey is t's generateContent tools[] key.
func GoogleWireKey(t GoogleTool) string { return googleWireKey[t] }
