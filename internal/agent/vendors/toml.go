package vendors

import (
	"encoding/json"
	"strings"
)

// tomlString reads one basic or literal TOML string value, dropping a trailing comment; bare values are kept.
func tomlString(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > 0 && (v[0] == '"' || v[0] == '\'') {
		if end := strings.IndexByte(v[1:], v[0]); end >= 0 {
			return v[1 : end+1]
		}
		return strings.Trim(v, `"'`)
	}
	v, _, _ = strings.Cut(v, "#")
	return strings.TrimSpace(v)
}

// TOMLQuote renders s as a TOML basic string (a JSON string is one) for a codex -c override.
func TOMLQuote(s string) string {
	b, _ := json.Marshal(s) // a JSON string is a valid TOML basic string
	return string(b)
}

func tomlInline(pairs [][2]string) string {
	parts := make([]string, len(pairs))
	for i, kv := range pairs {
		parts[i] = kv[0] + " = " + kv[1]
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}
