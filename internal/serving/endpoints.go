// Package serving owns Databricks serving-endpoint URL derivation and the served-models disk cache.
package serving

import "strings"

const (
	// ServingRoot is the Databricks workspace serving-endpoints path prefix.
	ServingRoot = "/serving-endpoints"
	// CatalogPath is the Databricks workspace serving-endpoints listing path.
	CatalogPath = "/api/2.0/serving-endpoints"
	// ProtocolAnthropic, ProtocolOpenAI, ProtocolResponses, ProtocolGemini, ProtocolInvocations name serving dialects.
	ProtocolAnthropic   = "anthropic"
	ProtocolOpenAI      = "openai"
	ProtocolResponses   = "responses"
	ProtocolGemini      = "gemini"
	ProtocolInvocations = "invocations"
)

// CatalogURL is host's /api/2.0/serving-endpoints URL, or "" when host is blank.
func CatalogURL(host string) string {
	host = strings.TrimRight(strings.TrimSpace(host), "/")
	if host == "" {
		return ""
	}
	return host + CatalogPath
}

// BaseURL derives the Databricks serving base URL for dialect from a workspace host, or "" when host is blank.
func BaseURL(host, dialect string) string {
	host = strings.TrimRight(strings.TrimSpace(host), "/")
	if host == "" {
		return ""
	}
	switch dialect {
	case ProtocolAnthropic:
		return host + ServingRoot + "/anthropic"
	case ProtocolGemini:
		return host + ServingRoot + "/gemini"
	case ProtocolOpenAI, ProtocolResponses, ProtocolInvocations:
		return host + ServingRoot
	}
	return ""
}

// ClientPath returns the serving request path under a dialect's base URL (including ?alt=sse for streamed Gemini).
func ClientPath(dialect, wireModel string, stream bool) string {
	switch dialect {
	case ProtocolAnthropic:
		return "/v1/messages"
	case ProtocolResponses:
		return "/responses"
	case ProtocolOpenAI:
		return "/chat/completions"
	case ProtocolGemini:
		if stream {
			return "/v1beta/models/" + wireModel + ":streamGenerateContent?alt=sse"
		}
		return "/v1beta/models/" + wireModel + ":generateContent"
	case ProtocolInvocations:
		return "/" + wireModel + "/invocations"
	}
	return ""
}

// EndpointURL joins base with dialect's serving client path; returns "" when base or path is empty.
func EndpointURL(base, dialect, wireModel string, stream bool) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	suffix := ClientPath(dialect, wireModel, stream)
	if base == "" || suffix == "" {
		return ""
	}
	return base + suffix
}

// DirectURL builds the full Databricks serving URL from a dynamic workspace host, dialect and wire model id.
func DirectURL(host, dialect, wireModel string, stream bool) string {
	return EndpointURL(BaseURL(host, dialect), dialect, wireModel, stream)
}
