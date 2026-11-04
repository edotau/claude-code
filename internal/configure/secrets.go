package configure

import (
	"regexp"
	"sort"
	"strings"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
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
	meta, metaErr := vault.LoadMeta()
	addCustomUsers(users, meta, metaErr)
	out := make([]TrackedSecret, 0, len(users))
	for name, u := range users {
		src, shadowed := locateSecret(name, meta, metaErr)
		out = append(out, TrackedSecret{Name: name, Source: src, Shadowed: shadowed, Users: strings.Join(u, ", ")})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// addCustomUsers marks every stored secret no provider reads as "custom".
func addCustomUsers(users map[string][]string, meta map[string]vault.SecretMeta, metaErr error) {
	var names []string
	if metaErr != nil {
		// Names reads vault names without decoding records, which may still work when metadata is unreadable.
		names = vault.Names()
	} else {
		for env := range meta {
			names = append(names, env)
		}
	}
	for _, env := range names {
		if _, ok := users[env]; !ok {
			users[env] = []string{"custom"}
		}
	}
}

// locateSecret resolves where name comes from today and any vault value an env var shadows.
func locateSecret(name string, meta map[string]vault.SecretMeta, metaErr error) (src, shadowed string) {
	if metaErr == nil {
		return vault.Locate(name, meta)
	}
	var err error
	if _, src, err = vault.EffectiveSecret(name); err != nil {
		src = "unreadable"
	} else if src == "env" {
		_, _, shadowed, _ = vault.Effective(name)
	}
	return src, shadowed
}

// ParseAssignment splits a pasted secret: `export NAME="value"`, `NAME=value`, or a bare value (name "").
// Surrounding quotes and whitespace are stripped; a bare value containing '=' (base64 padding) stays whole.
func ParseAssignment(raw string) (name, value string) {
	s := strings.TrimSpace(raw)
	s = strings.TrimSpace(strings.TrimPrefix(s, "export "))
	if k, v, ok := strings.Cut(s, "="); ok && strings.Trim(v, "=") != "" && EnvName.MatchString(k) && strings.ToUpper(k) == k {
		name, s = k, strings.TrimSpace(v)
	}
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		s = s[1 : len(s)-1]
	}
	return name, s
}
