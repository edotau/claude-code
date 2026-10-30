package agent

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
)

// secretFlagRE matches a CLI flag naming a secret (--key, --api-token=..., -password, ...), case-insensitive.
var secretFlagRE = regexp.MustCompile(`(?i)^--?[a-z0-9_-]*(key|token|secret|password)(=.*)?$`)

// RedactConfig blanks mcp.<name>.environment/headers values, provider.<name>.options.headers values,
// secret-flag values in .command, and the query string of .url in an OPENCODE_CONFIG_CONTENT string;
// returns s unchanged on any parse error.
func RedactConfig(s string) string {
	var cfg map[string]any
	if json.Unmarshal([]byte(s), &cfg) != nil {
		return s
	}
	redactMCP(cfg["mcp"])
	redactProviderOptions(cfg["provider"])
	body, err := json.Marshal(cfg)
	if err != nil {
		return s
	}
	return string(body)
}

// redactProviderOptions blanks provider.<name>.options.headers values in the decoded "provider" block;
// options.apiKey is left alone, it is already an "{env:...}" reference, never the literal secret.
func redactProviderOptions(raw any) {
	providers, ok := raw.(map[string]any)
	if !ok {
		return
	}
	for _, p := range providers {
		entry, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if opts, ok := entry["options"].(map[string]any); ok {
			redactConfigValues(opts["headers"])
		}
	}
}

func redactMCP(raw any) {
	servers, ok := raw.(map[string]any)
	if !ok {
		return
	}
	for _, s := range servers {
		entry, ok := s.(map[string]any)
		if !ok {
			continue
		}
		redactConfigValues(entry["environment"])
		redactConfigValues(entry["headers"])
		redactCommandSecrets(entry)
		redactURLQuery(entry)
	}
}

// redactConfigValues blanks every value of raw when it is a JSON object; anything else is left alone.
func redactConfigValues(raw any) {
	if m, ok := raw.(map[string]any); ok {
		for k := range m {
			m[k] = "<redacted>"
		}
	}
}

// redactCommandSecrets blanks the value following (or after "=" on) any secret-naming flag in entry["command"].
func redactCommandSecrets(entry map[string]any) {
	argv, ok := entry["command"].([]any)
	if !ok {
		return
	}
	for i := 0; i < len(argv); i++ {
		arg, ok := argv[i].(string)
		if !ok || !secretFlagRE.MatchString(arg) {
			continue
		}
		if idx := strings.IndexByte(arg, '='); idx >= 0 {
			argv[i] = arg[:idx+1] + "<redacted>"
			continue
		}
		if i+1 < len(argv) {
			argv[i+1] = "<redacted>"
			i++
		}
	}
}

// redactURLQuery strips entry["url"]'s query string, appending a marker so a redaction is visible.
func redactURLQuery(entry map[string]any) {
	raw, ok := entry["url"].(string)
	if !ok {
		return
	}
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery == "" {
		return
	}
	u.RawQuery = ""
	entry["url"] = u.String() + "?<redacted>"
}
