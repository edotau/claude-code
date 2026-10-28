package providers

// Auth types, the one auth.type value on disk and in memory.
const (
	AuthOAuth = "oauth" // browser login once; sealed {refresh, access}
	AuthM2M   = "m2m"   // client id + sealed client secret
	AuthPAT   = "pat"   // sealed personal access token
	AuthKey   = "key"   // a static key sent in a header
	AuthCLI   = "cli"   // the vendor CLI's own sign-in
	AuthNone  = "none"  // no credential
)

// Header styles for auth.header.
const (
	HeaderXAPIKey = "x-api-key"
	HeaderBearer  = "bearer"
)

// resolveKeyHeader fills a key row's header from the dialect: anthropic sends x-api-key, the rest bearer.
func (a *Auth) resolveKeyHeader(kind string) {
	if a.Type != AuthKey || a.Header != "" {
		return
	}
	a.Header = HeaderBearer
	if kind == ProtocolAnthropic {
		a.Header = HeaderXAPIKey
	}
}

// Method is the row's auth.type: oauth m2m pat key cli none; "" when unset.
func (a Auth) Method() string { return a.Type }

// HeaderStyle is the header a row sends its token in (x-api-key or bearer); "" for cli and none.
func (a Auth) HeaderStyle() string {
	switch a.Type {
	case AuthOAuth, AuthM2M, AuthPAT, AuthKey:
		if a.Header != "" {
			return a.Header
		}
		return HeaderBearer
	}
	return ""
}

// Sealed reports whether the credential lives in the vault (oauth, m2m or pat).
func (a Auth) Sealed() bool { return sealedAuth(a.Type) }
