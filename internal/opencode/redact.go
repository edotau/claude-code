package opencode

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
)

// secretFlagRE matches secret-naming CLI flags, case-insensitively (plus -H).
var secretFlagRE = regexp.MustCompile(`^((?i:--?[a-z0-9_-]*(key|token|secret|password|header|authorization|bearer|credential|pass))(=(?s:.*))?|-H)$`)

// redactConfig masks structured config secrets; parse errors leave the input unchanged.
func redactConfig(s string) string {
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

var envReferenceRE = regexp.MustCompile(`^\{env:[^{}]+\}$`)

// redactProviderOptions masks headers and literal API keys, preserving environment references.
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
			key, _ := opts["apiKey"].(string)
			if _, exists := opts["apiKey"]; exists && !envReferenceRE.MatchString(key) {
				opts["apiKey"] = "<redacted>"
			}
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
		redactOAuth(entry)
		redactCommandSecrets(entry)
		redactURLQuery(entry)
	}
}

// redactOAuth masks a literal oauth clientSecret, preserving environment references.
func redactOAuth(entry map[string]any) {
	oauth, ok := entry["oauth"].(map[string]any)
	if !ok {
		return
	}
	secret, exists := oauth["clientSecret"].(string)
	if exists && !envReferenceRE.MatchString(secret) {
		oauth["clientSecret"] = "<redacted>"
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

// redactURLQuery strips URL userinfo, query and fragment, marking removed query and fragment values.
func redactURLQuery(entry map[string]any) {
	raw, ok := entry["url"].(string)
	if !ok {
		return
	}
	u, err := url.Parse(raw)
	if err != nil { // a templated URL ("https://${HOST}/mcp?token=…") does not parse but still carries secrets
		entry["url"] = redactRawURL(raw)
		return
	}
	if u.RawQuery == "" && u.User == nil && u.Fragment == "" {
		return
	}
	marker := ""
	if u.RawQuery != "" {
		marker = "?<redacted>"
	}
	if u.Fragment != "" {
		marker += "#<redacted>"
	}
	u.RawQuery, u.User, u.Fragment, u.RawFragment = "", nil, "", ""
	entry["url"] = u.String() + marker
}

var rawUserinfoRE = regexp.MustCompile(`^([^:/?#]*://)[^/?#]*@`)

// redactRawURL is redactURLQuery's string-level form for a URL net/url rejects.
func redactRawURL(raw string) string {
	raw = rawUserinfoRE.ReplaceAllString(raw, "$1")
	marker := ""
	if i := strings.IndexByte(raw, '#'); i >= 0 {
		raw, marker = raw[:i], "#<redacted>"
	}
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		raw, marker = raw[:i], "?<redacted>"+marker
	}
	return raw + marker
}
