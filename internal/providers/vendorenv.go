package providers

// vendorEnvs maps provider + auth method to the env var that provider's vendor CLI reads its token from.
var vendorEnvs = map[string]map[string]string{
	"anthropic": {AuthOAuth: "CLAUDE_CODE_OAUTH_TOKEN", AuthCLI: "CLAUDE_CODE_OAUTH_TOKEN", AuthKey: "ANTHROPIC_API_KEY"},
	"github":    {AuthOAuth: "COPILOT_GITHUB_TOKEN", AuthPAT: "COPILOT_GITHUB_TOKEN"},
	"openai":    {AuthKey: "OPENAI_API_KEY"},
	"nvidia":    {AuthKey: "NVIDIA_API_KEY"},
	"google":    {AuthKey: "GEMINI_API_KEY"},
}

// VendorEnv is the env var a vendor CLI reads the token of a (provider, auth method) profile from; "" when it reads none.
func VendorEnv(provider, method string) string { return vendorEnvs[provider][method] }
