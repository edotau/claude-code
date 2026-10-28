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

// VendorEnvFor returns p's vendor env var and the new-style vault secret holding its token, both "" when the
// profile has no token a vendor reads. The env value is produced from the vault at export time.
func VendorEnvFor(p *Provider) (env, secret string) {
	method := p.Auth.Method()
	if env = VendorEnv(p.provider(), method); env == "" {
		return "", ""
	}
	if method == AuthCLI { // a cli row's setup token is sealed as its oauth secret
		method = AuthOAuth
	}
	return env, SecretName(p.provider(), p.Name, method)
}
