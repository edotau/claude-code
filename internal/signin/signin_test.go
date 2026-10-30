package signin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func env(t *testing.T) {
	t.Helper()
	vaulttest.UseKeyring(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
}

func row(name string, a providers.Auth) *providers.Provider {
	p := &providers.Provider{Name: name, Protocol: providers.ProtocolAnthropic, Auth: a}
	if a.Sealed() {
		p.Provider = providers.GroupDatabricks // sealed secret names derive from the provider key
	}
	return p
}

func TestForPicksKind(t *testing.T) {
	for _, c := range []struct {
		name string
		p    *providers.Provider
		want string
	}{
		{"u2m", row("a", providers.Auth{Type: providers.AuthOAuth}), "signin.u2m"},
		{"databricks preset no authType", &providers.Provider{Name: "b", Preset: providers.PresetDatabricks}, "signin.u2m"},
		{"pat", row("c", providers.Auth{Type: providers.AuthPAT}), "signin.secret"},
		{"oauth-m2m service principal", row("d", providers.Auth{Type: providers.AuthM2M, ClientID: "x"}), "signin.secret"},
		{"bearer env", row("e", providers.Auth{Type: providers.AuthKey, Env: "E_KEY"}), "signin.secret"},
		{"x-api-key env", row("f", providers.Auth{Type: "x-api-key", Env: "F_KEY"}), "signin.secret"},
		{"subscription", row("g", providers.Auth{Type: providers.AuthCLI}), "signin.claude"},
		{"codex", row("h", providers.Auth{Type: providers.AuthCLI, Login: "codex"}), "signin.vendor"},
		{"copilot", row("i", providers.Auth{Type: providers.AuthCLI, Login: "copilot"}), "signin.vendor"},
		{"gemini", row("j", providers.Auth{Type: providers.AuthCLI, Login: "gemini"}), "signin.vendor"},
		{"none/ollama", row("k", providers.Auth{Type: providers.AuthNone}), "signin.none"},
		{"method oauth", row("l", providers.Auth{Type: providers.AuthOAuth}), "signin.u2m"},
		{"method m2m", row("m", providers.Auth{Type: providers.AuthM2M}), "signin.secret"},
		{"method pat", row("n", providers.Auth{Type: providers.AuthPAT}), "signin.secret"},
		{"method key", row("o", providers.Auth{Type: providers.AuthKey}), "signin.secret"},
		{"method cli", row("p", providers.Auth{Type: providers.AuthCLI}), "signin.claude"},
		{"method cli login", row("q", providers.Auth{Type: providers.AuthCLI, Login: "codex"}), "signin.vendor"},
	} {
		if got := fmt.Sprintf("%T", For(c.p)); got != c.want {
			t.Errorf("%s: For = %s, want %s", c.name, got, c.want)
		}
	}
}

func TestCheckVerdictsPerKind(t *testing.T) {
	env(t)
	ocred := credential
	t.Cleanup(func() { credential = ocred })
	oc, og, ov, ocp := claudeStatus, geminiPresent, codexStatus, copilotLogin
	t.Cleanup(func() { claudeStatus, geminiPresent, codexStatus, copilotLogin = oc, og, ov, ocp })
	pathstest.FakeVendor(t, "codex", "#!/bin/sh\nexit 0\n")

	var credErr error
	credential = func(context.Context, *providers.Provider) (string, error) { return "tok", credErr }
	t.Setenv("E_KEY", "present")

	u2mRow := row("u", providers.Auth{Type: providers.AuthOAuth})
	envRow := row("e", providers.Auth{Type: providers.AuthKey, Env: "E_KEY"})
	t.Run("u2m", func(t *testing.T) {
		for err, want := range map[string]providers.AuthVerdict{"": providers.VerdictOK, "login": providers.VerdictNeedsLogin, "net": providers.VerdictTransient} {
			credErr = map[string]error{"": nil, "login": providers.ErrNeedsLogin, "net": errors.New("dial tcp: timeout")}[err]
			if v, _ := For(u2mRow).Check(context.Background()); v != want {
				t.Errorf("u2m %q = %s, want %s", err, v, want)
			}
		}
	})
	t.Run("secret env", func(t *testing.T) {
		credErr = nil
		if v, _ := For(envRow).Check(context.Background()); v != providers.VerdictOK {
			t.Errorf("present env secret = %s", v)
		}
		credErr = errors.New("503")
		if v, _ := For(envRow).Check(context.Background()); v != providers.VerdictTransient {
			t.Errorf("transient mint = %s", v)
		}
		t.Setenv("E_KEY", "")
		if v, d := For(envRow).Check(context.Background()); v != providers.VerdictNeedsLogin || d != "no credential" {
			t.Errorf("missing secret = %s %q", v, d)
		}
	})
	t.Run("sealed missing", func(t *testing.T) {
		pat := row("pat1", providers.Auth{Type: providers.AuthPAT})
		if v, d := For(pat).Check(context.Background()); v != providers.VerdictNeedsLogin || d != "no credential" {
			t.Errorf("sealed missing = %s %q", v, d)
		}
	})
	t.Run("subscription", func(t *testing.T) {
		sub := row("s", providers.Auth{Type: providers.AuthCLI})
		claudeStatus = func() (bool, string, error) { return true, "(max)", nil }
		if v, _ := For(sub).Check(context.Background()); v != providers.VerdictOK {
			t.Errorf("logged in = %s", v)
		}
		claudeStatus = func() (bool, string, error) { return false, "", nil }
		if v, _ := For(sub).Check(context.Background()); v != providers.VerdictNeedsLogin {
			t.Errorf("logged out = %s", v)
		}
	})
	t.Run("copilot", func(t *testing.T) {
		cp := row("c", providers.Auth{Type: providers.AuthCLI, Login: "copilot"})
		copilotLogin = func() string { return "octo" }
		if v, d := For(cp).Check(context.Background()); v != providers.VerdictOK || !strings.Contains(d, "octo") {
			t.Errorf("signed in = %s %q", v, d)
		}
		copilotLogin = func() string { return "" }
		if v, _ := For(cp).Check(context.Background()); v != providers.VerdictNeedsLogin {
			t.Errorf("signed out = %s", v)
		}
	})
	t.Run("gemini", func(t *testing.T) {
		g := row("g", providers.Auth{Type: providers.AuthCLI, Login: "gemini"})
		for present, want := range map[bool]providers.AuthVerdict{true: providers.VerdictOK, false: providers.VerdictNeedsLogin} {
			geminiPresent = func() bool { return present }
			if v, _ := For(g).Check(context.Background()); v != want {
				t.Errorf("present=%v = %s", present, v)
			}
		}
	})
	t.Run("codex", func(t *testing.T) {
		cx := row("x", providers.Auth{Type: providers.AuthCLI, Login: "codex"})
		codexStatus = func(context.Context, string) ([]byte, error) { return []byte("Logged in using ChatGPT\n"), nil }
		if v, d := For(cx).Check(context.Background()); v != providers.VerdictOK || d != "Logged in using ChatGPT" {
			t.Errorf("logged in = %s %q", v, d)
		}
		codexStatus = func(context.Context, string) ([]byte, error) {
			return nil, &exec.ExitError{}
		}
		if v, _ := For(cx).Check(context.Background()); v != providers.VerdictNeedsLogin {
			t.Errorf("exit 1 = %s", v)
		}
		codexStatus = func(context.Context, string) ([]byte, error) { return nil, errors.New("exec: killed") }
		if v, _ := For(cx).Check(context.Background()); v != providers.VerdictTransient {
			t.Errorf("non-exit error must be transient, got %s", v)
		}
	})
}

func TestNoneCheckReachability(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := up.URL
	p := &providers.Provider{Name: "ollama", BaseURL: url, Auth: providers.Auth{Type: providers.AuthNone}}
	if v, _ := For(p).Check(context.Background()); v != providers.VerdictOK {
		t.Errorf("reachable = %s", v)
	}
	up.Close()
	if v, _ := For(p).Check(context.Background()); v != providers.VerdictTransient {
		t.Errorf("down must be transient (never needs login), got %s", v)
	}
	if err := For(p).Login(context.Background(), nil, io.Discard); err != nil {
		t.Errorf("none Login must be a no-op: %v", err)
	}
}

// A 401 invalid_client from the token endpoint is rejected, not needs_login and not transient.
func TestSecretCheckRejectedOn401(t *testing.T) {
	env(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_client", "error_description": "bad"})
	}))
	t.Cleanup(srv.Close)
	p := &providers.Provider{Name: "sp-rej", Provider: providers.GroupDatabricks, Protocol: providers.ProtocolAnthropic, BaseURL: srv.URL + "/serving-endpoints/anthropic",
		Auth: providers.Auth{Type: providers.AuthM2M, ClientID: "cid", TTL: 1800}}
	if err := providers.SealSecret(providers.AuthSecretName(p), "s3cret-value"); err != nil {
		t.Fatal(err)
	}
	v, d := For(p).Check(context.Background())
	if v != providers.VerdictRejected {
		t.Fatalf("verdict = %s (%q), want rejected", v, d)
	}
	if strings.Contains(d, "s3cret-value") {
		t.Errorf("detail leaks the secret: %q", d)
	}
}

func TestSecretLogin(t *testing.T) {
	env(t)
	p := row("pat-row", providers.Auth{Type: providers.AuthPAT})
	name := providers.AuthSecretName(p)
	otty, opr, ost := stdinTTY, promptSecret, storeSecret
	t.Cleanup(func() { stdinTTY, promptSecret, storeSecret = otty, opr, ost })
	var stored = map[string]string{}
	storeSecret = func(n, v string) error { stored[n] = v; return nil }
	prompts := 0
	promptSecret = func(string) (string, error) { prompts++; return " dapi-new-value ", nil }

	stdinTTY = func() bool { return false }
	err := For(p).Login(context.Background(), nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "claude-code vault set "+name) || prompts != 0 || len(stored) != 0 {
		t.Fatalf("non-TTY: err=%v prompts=%d stored=%v", err, prompts, stored)
	}

	stdinTTY = func() bool { return true }
	if err := For(p).Login(context.Background(), nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if stored[name] != "dapi-new-value" {
		t.Errorf("stored %v, want trimmed value under %s", stored, name)
	}

	promptSecret = func(string) (string, error) { return "   ", nil }
	stored = map[string]string{}
	if err := For(p).Login(context.Background(), nil, io.Discard); err == nil || len(stored) != 0 {
		t.Errorf("empty value must fail without storing: %v %v", err, stored)
	}
}

func TestVendorLogin(t *testing.T) {
	orv := runVendor
	t.Cleanup(func() { runVendor = orv })
	var calls []string
	runVendor = func(_ context.Context, name string, args []string, _ io.Reader, _ io.Writer) error {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil
	}
	for login, want := range map[string]string{"codex": "codex login", "copilot": "copilot login"} {
		calls = nil
		if err := For(row(login, providers.Auth{Type: providers.AuthCLI, Login: login})).Login(context.Background(), nil, io.Discard); err != nil || len(calls) != 1 || calls[0] != want {
			t.Errorf("%s: err=%v calls=%v want %q", login, err, calls, want)
		}
	}
	calls = nil
	if err := For(row("sub", providers.Auth{Type: providers.AuthCLI})).Login(context.Background(), nil, io.Discard); err != nil || len(calls) != 1 || calls[0] != "claude auth login" {
		t.Errorf("subscription: err=%v calls=%v", err, calls)
	}
	calls = nil
	err := For(row("g", providers.Auth{Type: providers.AuthCLI, Login: "gemini"})).Login(context.Background(), nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "agy") || len(calls) != 0 {
		t.Errorf("gemini must error without running a CLI: %v %v", err, calls)
	}
}

func TestU2MLoginUsesLoginRow(t *testing.T) {
	orw := loginRow
	t.Cleanup(func() { loginRow = orw })
	var got string
	loginRow = func(_ context.Context, name string, _ io.Writer) error { got = name; return nil }
	var buf bytes.Buffer
	if err := For(row("qa", providers.Auth{Type: providers.AuthOAuth})).Login(context.Background(), nil, &buf); err != nil || got != "qa" {
		t.Fatalf("err=%v got=%q", err, got)
	}
}

func TestValidateSecret(t *testing.T) {
	if err := ValidateSecret(agent.CopilotTokenEnv, "not-a-token"); err == nil {
		t.Error("a non-GitHub token must be rejected for the copilot env name")
	}
	if err := ValidateSecret(agent.CopilotTokenEnv, "ghp_"+strings.Repeat("a", 30)); err != nil {
		t.Errorf("valid copilot token rejected: %v", err)
	}
	if err := ValidateSecret("SOME_KEY", "value"); err != nil {
		t.Errorf("plain secret rejected: %v", err)
	}
}

func TestClaudeProbeFailureIsTransient(t *testing.T) {
	o := claudeStatus
	t.Cleanup(func() { claudeStatus = o })
	sub := row("s", providers.Auth{Type: providers.AuthCLI})
	claudeStatus = func() (bool, string, error) { return false, "", errors.New("exec: not found") }
	v, d := For(sub).Check(context.Background())
	if v != providers.VerdictTransient || !strings.Contains(d, "claude auth status failed") {
		t.Errorf("probe failure = %s %q, want transient", v, d)
	}
	claudeStatus = func() (bool, string, error) { return false, "", nil }
	if v, _ := For(sub).Check(context.Background()); v != providers.VerdictNeedsLogin {
		t.Errorf("definite logged-out = %s", v)
	}
}

func TestU2MCliProfileCommandFailure(t *testing.T) {
	env(t)
	ocred, orow := credential, loginRow
	t.Cleanup(func() { credential, loginRow = ocred, orow })
	cli := &providers.Provider{Name: "cli-row", Preset: providers.PresetDatabricks, CLIProfile: "qa-prof"}
	sealed := row("u-row", providers.Auth{Type: providers.AuthOAuth})
	for _, c := range []struct {
		name string
		p    *providers.Provider
		err  error
		hint bool
	}{
		{"cliProfile command fails", cli, errors.New("exit status 1"), true},
		{"cliProfile timeout", cli, context.DeadlineExceeded, true},
		{"cliProfile canceled", cli, fmt.Errorf("wrap: %w", context.Canceled), true},
		{"sealed u2m transient", sealed, errors.New("dial tcp"), false},
	} {
		credential = func(context.Context, *providers.Provider) (string, error) { return "", c.err }
		v, d := For(c.p).Check(context.Background())
		if v != providers.VerdictTransient {
			t.Errorf("%s = %s, want transient (never promoted to needs_login)", c.name, v)
		}
		if got := strings.Contains(d, "databricks auth login -p qa-prof"); got != c.hint {
			t.Errorf("%s: detail %q hint=%v want %v", c.name, d, got, c.hint)
		}
	}
}

func TestU2MLoginCliProfileNeverCallsLoginRow(t *testing.T) {
	orw := loginRow
	t.Cleanup(func() { loginRow = orw })
	var calls []string
	loginRow = func(_ context.Context, n string, _ io.Writer) error { calls = append(calls, n); return nil }
	cli := &providers.Provider{Name: "cli-row", Preset: providers.PresetDatabricks, CLIProfile: "qa-prof"}
	err := For(cli).Login(context.Background(), nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "databricks auth login -p qa-prof") || len(calls) != 0 {
		t.Fatalf("cliProfile Login: err=%v loginRow calls=%v", err, calls)
	}
	sealed := row("u-row", providers.Auth{Type: providers.AuthOAuth})
	if err := For(sealed).Login(context.Background(), nil, io.Discard); err != nil || len(calls) != 1 || calls[0] != "u-row" {
		t.Fatalf("sealed u2m Login: err=%v calls=%v", err, calls)
	}
}

// An unreadable vault is not "no credential": it must stay transient, never prompt a login.
func TestSecretCheckVaultReadErrorIsTransient(t *testing.T) {
	env(t)
	if err := os.MkdirAll(filepath.Dir(vault.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vault.Path(), bytes.Repeat([]byte("garbage"), 1000), 0o600); err != nil {
		t.Fatal(err)
	}
	pat := row("pat-v", providers.Auth{Type: providers.AuthPAT})
	t.Setenv(providers.AuthSecretName(pat), "")
	v, d := For(pat).Check(context.Background())
	if v != providers.VerdictTransient || !strings.HasPrefix(d, "vault: ") {
		t.Fatalf("unreadable vault = %s %q, want transient with a vault: detail", v, d)
	}
}

// The codex status line may arrive on stderr; only its first line, cut at " - ", becomes the detail.
func TestCodexDetailFromCombinedOutput(t *testing.T) {
	env(t)
	pathstest.FakeVendor(t, "codex", "#!/bin/sh\necho 'Logged in using ChatGPT - me@example.com' >&2\necho second line\n")
	v, d := For(row("x", providers.Auth{Type: providers.AuthCLI, Login: "codex"})).Check(context.Background())
	if v != providers.VerdictOK || d != "Logged in using ChatGPT" {
		t.Errorf("stderr-only status = %s %q", v, d)
	}
}

func TestSecretOKDetail(t *testing.T) {
	env(t)
	ocred := credential
	t.Cleanup(func() { credential = ocred })
	credential = func(context.Context, *providers.Provider) (string, error) { return "tok", nil }
	t.Setenv("E_KEY", "present")
	if v, d := For(row("e", providers.Auth{Type: providers.AuthKey, Env: "E_KEY"})).Check(context.Background()); v != providers.VerdictOK || d != "key present, not validated" {
		t.Errorf("env key = %s %q", v, d)
	}
	pat := row("pat2", providers.Auth{Type: providers.AuthPAT})
	if err := providers.SealSecret(providers.AuthSecretName(pat), "dapi-x"); err != nil {
		t.Fatal(err)
	}
	if v, d := For(pat).Check(context.Background()); v != providers.VerdictOK || d != "" {
		t.Errorf("sealed pat (validated by mint) = %s %q, want ok with no caveat", v, d)
	}
}

func TestCodexDetailCutAtDash(t *testing.T) {
	env(t)
	o := codexStatus
	t.Cleanup(func() { codexStatus = o })
	pathstest.FakeVendor(t, "codex", "#!/bin/sh\nexit 0\n")
	codexStatus = func(context.Context, string) ([]byte, error) {
		return []byte("Logged in using ChatGPT - user@example.com\n"), nil
	}
	_, d := For(row("x", providers.Auth{Type: providers.AuthCLI, Login: "codex"})).Check(context.Background())
	if d != "Logged in using ChatGPT" {
		t.Errorf("detail = %q, want the part before \" - \"", d)
	}
}

func TestValidateSecretGitHubVaultNames(t *testing.T) {
	for _, n := range []string{agent.CopilotTokenEnv, "GITHUB_PERSONAL_PAT", "GITHUB_PAT"} {
		if ValidateSecret(n, "not-a-token") == nil {
			t.Errorf("%s accepted a non-GitHub token", n)
		}
		if err := ValidateSecret(n, "ghp_"+strings.Repeat("a", 30)); err != nil {
			t.Errorf("%s rejected a valid token: %v", n, err)
		}
	}
	if err := ValidateSecret("DATABRICKS_PROD_PAT", "dapi-anything"); err != nil {
		t.Errorf("non-GitHub PAT name must not be shape-checked: %v", err)
	}
}
