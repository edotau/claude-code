package providers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ErrPassthrough means the provider forwards the client's own auth; there is no credential to mint.
var ErrPassthrough = errors.New("provider uses passthrough auth")

type cached struct {
	value string
	until time.Time
}

var cmdCache sync.Map // provider name → cached

// Credential resolves the provider's secret: env var → env.d/secrets.env → file → command (cached for TTL).
func Credential(ctx context.Context, p *Provider) (string, error) {
	switch p.Auth.Type {
	case AuthNone:
		return "", nil
	case AuthPassthrough:
		return "", ErrPassthrough
	}
	if p.Auth.Env != "" {
		if v := strings.TrimSpace(os.Getenv(p.Auth.Env)); v != "" {
			return v, nil
		}
		if v := strings.TrimSpace(ReadEnvFile(SecretsFile())[p.Auth.Env]); v != "" {
			return v, nil
		}
	}
	if p.Auth.File != "" {
		if b, err := os.ReadFile(expandHome(p.Auth.File)); err == nil {
			if v := strings.TrimSpace(string(b)); v != "" {
				return v, nil
			}
		}
	}
	if p.Auth.Command != "" {
		return commandCredential(ctx, p, false)
	}
	return "", fmt.Errorf("%s: no credential (set %s in the env or %s%s)", p.Name, orDash(p.Auth.Env), SecretsFile(), hint(p))
}

// Refresh drops a cached command credential and re-runs it; used after an upstream 401.
func Refresh(ctx context.Context, p *Provider) (string, error) {
	if p.Auth.Command == "" {
		return Credential(ctx, p)
	}
	return commandCredential(ctx, p, true)
}

func commandCredential(ctx context.Context, p *Provider, force bool) (string, error) {
	if c, ok := cmdCache.Load(p.Name); ok && !force && time.Now().Before(c.(cached).until) {
		return c.(cached).value, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sh", "-c", p.Auth.Command).Output()
	if err != nil {
		return "", fmt.Errorf("%s: auth command failed: %w", p.Name, err)
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		return "", fmt.Errorf("%s: auth command printed nothing", p.Name)
	}
	ttl := time.Duration(p.Auth.TTL) * time.Second
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	cmdCache.Store(p.Name, cached{value: v, until: time.Now().Add(ttl)})
	return v, nil
}

// CredentialPresent reports whether Credential would succeed without running a command.
func CredentialPresent(p *Provider) bool {
	switch p.Auth.Type {
	case AuthNone, AuthPassthrough:
		return true
	}
	if p.Auth.Command != "" {
		return true
	}
	v, err := Credential(context.Background(), p)
	return err == nil && v != ""
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

func orDash(s string) string {
	if s == "" {
		return "auth.env"
	}
	return s
}

func hint(p *Provider) string {
	if p.Auth.File != "" {
		return ", or " + p.Auth.File
	}
	return ""
}
