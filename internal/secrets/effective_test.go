package secrets

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

// fakeSecretTool points PATH at a stub `secret-tool`: `lookup service claude-code account <name>`
// answers table[name] and exits 0, anything else exits 1 (a plain miss, same as the real tool).
func fakeSecretTool(t *testing.T, table map[string]string) {
	t.Helper()
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString("#!/bin/sh\nif [ \"$1\" = lookup ]; then\n  case \"$5\" in\n")
	for k, v := range table {
		b.WriteString("    " + k + ") printf '%s' '" + v + "'; exit 0 ;;\n")
	}
	b.WriteString("    *) exit 1 ;;\n  esac\nfi\nexit 1\n")
	if err := os.WriteFile(filepath.Join(dir, "secret-tool"), []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

// With nothing but the keyring holding a value, Effective must report it unshadowed.
func TestEffectiveKeyringOnly(t *testing.T) {
	setConfigDir(t)
	fakeSecretTool(t, map[string]string{"KR_ONLY": "kr-value"})

	v, src, shadowed, ok := Effective("KR_ONLY")
	if !ok || v != "kr-value" || src != "keyring" || shadowed != "" {
		t.Fatalf("Effective = (%q, %q, %q, %v), want (kr-value, keyring, \"\", true)", v, src, shadowed, ok)
	}
}

// An env var with the same name also held in the keyring must win, flagged as shadowing it —
// while Get (the managed-ladder lookup `secrets get` uses) still prefers the keyring.
func TestEffectiveEnvOverKeyring(t *testing.T) {
	setConfigDir(t)
	fakeSecretTool(t, map[string]string{"ENV_OVER_KR": "kr-value"})
	t.Setenv("ENV_OVER_KR", "env-value")

	v, src, shadowed, ok := Effective("ENV_OVER_KR")
	if !ok || v != "env-value" || src != "env" || shadowed != "keyring" {
		t.Fatalf("Effective = (%q, %q, %q, %v), want (env-value, env, keyring, true)", v, src, shadowed, ok)
	}
	if gv, gsrc, gok := Get("ENV_OVER_KR"); !gok || gv != "kr-value" || gsrc != "keyring" {
		t.Fatalf("Get = (%q, %q, %v), want (kr-value, keyring, true)", gv, gsrc, gok)
	}
}

// An env var shadowing only the file plane (no keyring) must flag shadowed=file.
func TestEffectiveEnvOverFile(t *testing.T) {
	setConfigDir(t)
	noKeyring(t)
	if err := setFile("ENV_OVER_FILE", "file-value"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENV_OVER_FILE", "env-value")

	v, src, shadowed, ok := Effective("ENV_OVER_FILE")
	if !ok || v != "env-value" || src != "env" || shadowed != "file" {
		t.Fatalf("Effective = (%q, %q, %q, %v), want (env-value, env, file, true)", v, src, shadowed, ok)
	}
}

// A name held nowhere must miss cleanly.
func TestEffectiveMiss(t *testing.T) {
	setConfigDir(t)
	noKeyring(t)

	v, src, shadowed, ok := Effective("NEVER_SET_XYZ")
	if ok || v != "" || src != "" || shadowed != "" {
		t.Fatalf("Effective(miss) = (%q, %q, %q, %v), want all empty, ok=false", v, src, shadowed, ok)
	}
}

// Cross-caller contract: with the same name in env and keyring, providers.Credential, secrets.Resolve
// and secrets.Effective must all agree the source is env, while secrets.Get still returns the keyring value.
func TestCrossCallerAgreeEnvWinsButGetPrefersKeyring(t *testing.T) {
	setConfigDir(t)
	fakeSecretTool(t, map[string]string{"CROSS_NAME": "kr-value"})
	t.Setenv("CROSS_NAME", "env-value")

	p := &providers.Provider{
		Name: "x", Kind: providers.DialectAnthropic,
		Auth: providers.Auth{Type: providers.AuthAPIKey, Env: "CROSS_NAME"},
	}
	cv, err := providers.Credential(context.Background(), p)
	if err != nil || cv != "env-value" {
		t.Fatalf("providers.Credential = (%q, %v), want (env-value, nil)", cv, err)
	}
	if st := Resolve(p); st.Source != "env" {
		t.Fatalf("Resolve(p).Source = %q, want env", st.Source)
	}
	if _, src, _, ok := Effective("CROSS_NAME"); !ok || src != "env" {
		t.Fatalf("Effective source = %q (ok=%v), want env", src, ok)
	}
	if v, src, ok := Get("CROSS_NAME"); !ok || v != "kr-value" || src != "keyring" {
		t.Fatalf("Get = (%q, %q, %v), want (kr-value, keyring, true)", v, src, ok)
	}
}
