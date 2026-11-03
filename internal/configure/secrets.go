package configure

import (
	"regexp"
	"sort"
	"strings"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secretstore"
)

// EnvName is a valid secret name: an env var identifier.
var EnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// TrackedSecret is one row of the secrets dialog: a name the harness knows of and where it resolves today.
type TrackedSecret struct {
	Name     string
	Source   string // vault|env|"" (missing)
	Shadowed string // vault|"" — a stored value an env var is shadowing
	Users    string // providers that read it, or a short role
}

// TrackedSecrets combines provider auth.env names, the setup-token, and every secret stored in the vault.
func TrackedSecrets(reg *providers.Registry) []TrackedSecret {
	users := map[string][]string{
		providers.OAuthTokenEnv: {"claude -p (setup-token)"},
	}
	for _, n := range reg.Names() {
		if env := reg.Providers[n].Auth.Env; env != "" {
			users[env] = append(users[env], n)
		}
	}
	meta, metaErr := secretstore.LoadMeta()
	if metaErr != nil {
		// Names reads vault names without decoding records, which may still work when metadata is unreadable.
		for _, env := range secretstore.Names() {
			if _, ok := users[env]; !ok {
				users[env] = []string{"custom"}
			}
		}
	} else {
		for env := range meta {
			if _, ok := users[env]; !ok {
				users[env] = []string{"custom"}
			}
		}
	}
	out := make([]TrackedSecret, 0, len(users))
	for name, u := range users {
		var src, shadowed string
		if metaErr == nil {
			src, shadowed = secretstore.Locate(name, meta)
		} else {
			var err error
			if _, src, err = secretstore.EffectiveSecret(name); err != nil {
				src = "unreadable"
			} else if src == "env" {
				_, _, shadowed, _ = secretstore.Effective(name)
			}
		}
		out = append(out, TrackedSecret{Name: name, Source: src, Shadowed: shadowed, Users: strings.Join(u, ", ")})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ParseAssignment splits a pasted secret: `export NAME="value"`, `NAME=value`, or a bare value (name "").
// Surrounding quotes and whitespace are stripped; a bare value containing '=' (base64 padding) stays whole.
func ParseAssignment(raw string) (name, value string) {
	s := strings.TrimSpace(raw)
	s = strings.TrimSpace(strings.TrimPrefix(s, "export "))
	if k, v, ok := strings.Cut(s, "="); ok && v != "" && EnvName.MatchString(k) && strings.ToUpper(k) == k {
		name, s = k, strings.TrimSpace(v)
	}
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		s = s[1 : len(s)-1]
	}
	return name, s
}
