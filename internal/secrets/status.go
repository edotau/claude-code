package secrets

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/providers"
)

// ProviderStatus is one provider's credential state: which source would serve it today, never the
// value itself.
type ProviderStatus struct {
	Name   string
	Type   string
	Source string // env | vault | auth.file | command | passthrough | none
	Note   string
}

// Resolve reports credential presence in env -> vault -> auth.file -> command order, without resolving a key.
func Resolve(p *providers.Provider) ProviderStatus {
	st := ProviderStatus{Name: p.Name, Type: p.Auth.Type}
	switch p.Auth.Type {
	case providers.AuthNone:
		st.Source = "none"
		st.Note = "no credential needed"
		return st
	case providers.AuthPassthrough:
		st.Source = "passthrough"
		if p.Auth.Login != "" {
			st.Note = "via `" + p.Auth.Login + "` login"
		}
		return st
	}
	if p.Auth.Env != "" {
		source, err := providers.HasSecretErr(p.Auth.Env)
		st.Source = source
		if err != nil {
			st.Note = "vault unreadable: " + err.Error()
		}
	}
	if st.Source == "" && p.Auth.File != "" {
		if b, err := os.ReadFile(expandHome(p.Auth.File)); err == nil && strings.TrimSpace(string(b)) != "" {
			st.Source = "auth.file"
		}
	}
	if st.Source == "" && p.Auth.Command != "" {
		st.Source = "command"
	}
	if st.Source == "" {
		st.Source = "none"
		if p.Auth.Env != "" && st.Note == "" {
			st.Note = "set " + p.Auth.Env
		}
	}
	return st
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return h + p[1:]
		}
	}
	return p
}

// Status prints every registered provider's credential source, never a value.
func Status(out io.Writer, reg *providers.Registry) {
	fmt.Fprintln(out, "== credential status ==")
	for _, name := range reg.Names() {
		p := reg.Providers[name]
		st := Resolve(p)
		line := fmt.Sprintf("  %-16s %-12s %s", st.Name, st.Type, st.Source)
		if st.Note != "" {
			line += "  (" + st.Note + ")"
		}
		fmt.Fprintln(out, line)
	}
}
