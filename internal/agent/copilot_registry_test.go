package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/copilotreg"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// --model in both spellings is lifted out in order; every other arg (including -m and a trailing bare --model) stays.
func TestSplitModelFlags(t *testing.T) {
	for _, tc := range []struct {
		name         string
		args         []string
		models, rest []string
	}{
		{"none", []string{"-p", "hi"}, nil, []string{"-p", "hi"}},
		{"space form", []string{"--model", "a", "-p", "hi"}, []string{"a"}, []string{"-p", "hi"}},
		{"equals form", []string{"--model=a", "x"}, []string{"a"}, []string{"x"}},
		{"repeated keeps order", []string{"--model", "a", "y", "--model=b"}, []string{"a", "b"}, []string{"y"}},
		{"trailing bare flag stays", []string{"x", "--model"}, nil, []string{"x", "--model"}},
		{"short -m is not a model flag", []string{"-m", "a"}, nil, []string{"-m", "a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			models, rest := splitModelFlags(tc.args)
			if !slices.Equal(models, tc.models) || !slices.Equal(rest, tc.rest) {
				t.Errorf("splitModelFlags(%q) = %q, %q; want %q, %q", tc.args, models, rest, tc.models, tc.rest)
			}
		})
	}
}

// The private home gets only shared entries as links, settings.json without "model", never config.json or
// ucode.env, and 0700 dirs; a second seed refreshes the links in place.
func TestSeedCopilotHome(t *testing.T) {
	root := t.TempDir()
	src, home, xdg := filepath.Join(root, "src"), filepath.Join(root, "home"), filepath.Join(root, "xdg")
	for _, d := range []string{"agents", "skills"} {
		if err := os.MkdirAll(filepath.Join(src, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"settings.json": `{"model":"gpt-x","theme":"dark"}`, "config.json": `{"loggedInUsers":[{"login":"u"}]}`,
		"ucode.env": "TOKEN=bearer", "mcp-config.json": "{}", "history.db": "x",
	}
	for n, body := range files {
		if err := os.WriteFile(filepath.Join(src, n), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		warns, err := seedCopilotHome(src, home, xdg)
		if err != nil || len(warns) != 0 {
			t.Fatalf("seed = %v, %v", warns, err)
		}
	}
	for _, d := range []string{home, xdg} {
		if fi, err := os.Stat(d); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("%s = %v, %v; want a 0700 dir", d, fi, err)
		}
	}
	for _, n := range []string{"agents", "skills", "mcp-config.json"} {
		if got, err := os.Readlink(filepath.Join(home, n)); err != nil || got != filepath.Join(src, n) {
			t.Errorf("%s link = %q, %v; want -> %s", n, got, err, filepath.Join(src, n))
		}
	}
	for _, n := range []string{"config.json", "ucode.env", "history.db"} {
		if _, err := os.Lstat(filepath.Join(home, n)); err == nil {
			t.Errorf("%s was carried into the private home", n)
		}
	}
	raw, err := os.ReadFile(filepath.Join(home, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if _, has := got["model"]; has || got["theme"] != "dark" {
		t.Errorf("settings.json = %v; want theme kept, model dropped", got)
	}
}

// A GitHub login in the private home (e.g. after /login) refuses the launch; a login-free config.json with the
// leading // comments copilot writes does not.
func TestSeedRefusesGitHubLogin(t *testing.T) {
	root := t.TempDir()
	home, xdg := filepath.Join(root, "home"), filepath.Join(root, "xdg")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(home, "config.json")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("// User settings\n// do not edit\n" + `{"loggedInUsers":[],"theme":"dark"}`)
	if _, err := seedCopilotHome("", home, xdg); err != nil {
		t.Fatalf("login-free config refused: %v", err)
	}
	for _, body := range []string{
		"// c\n" + `{"loggedInUsers":[{"host":"https://github.com","login":"u"}]}`,
		`{"copilotTokens":{"github.com:u":"gho_x"}}`,
		`{not json`,
	} {
		write(body)
		if _, err := seedCopilotHome("", home, xdg); err == nil {
			t.Errorf("launch not refused for config.json %q", body)
		}
	}
}

func registryTarget() providers.Target {
	p := &providers.Provider{Name: "dbx", Provider: providers.GroupDatabricks, Preset: providers.PresetDatabricks,
		Auth: providers.Auth{Type: providers.AuthKey, Env: "X"}}
	return providers.Target{Provider: p, Model: "databricks-claude-sonnet-5-5"}
}

// registryHarness isolates config/home/runtime dir and stubs the three boundaries; it returns the revoked tokens.
func registryHarness(t *testing.T, mint func() (string, error)) (minted *int, revoked *[]string) {
	t.Helper()
	vaulttest.Use(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("COPILOT_HOME", "")
	shm, err := os.MkdirTemp("/dev/shm", "p34-rt-") // the registry dir must sit on tmpfs
	if err != nil {
		t.Skipf("no tmpfs for the registry dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shm) })
	t.Setenv("XDG_RUNTIME_DIR", shm)
	od, om, orv := discoverModels, mintSession, revokeSession
	t.Cleanup(func() { discoverModels, mintSession, revokeSession = od, om, orv })
	discoverModels = func(context.Context, *providers.Registry, *providers.Provider) ([]copilotreg.Endpoint, error) {
		return []copilotreg.Endpoint{{ID: "databricks-claude-sonnet-5-5", Name: "S"}}, nil
	}
	minted, revoked = new(int), new([]string)
	mintSession = func(context.Context, string, int) (string, error) { *minted++; return mint() }
	revokeSession = func(_ context.Context, tok string) error { *revoked = append(*revoked, tok); return nil }
	return minted, revoked
}

// The registry launch fails closed: a mint failure, a registry-write failure and an unregistered --model each
// return an error (never a launchable CopilotRegistry), and any minted token is revoked.
func TestConfigureCopilotRegistryFailsClosed(t *testing.T) {
	ctx := context.Background()

	t.Run("mint failure", func(t *testing.T) {
		minted, revoked := registryHarness(t, func() (string, error) { return "", errors.New("daemon down") })
		cr, err := ConfigureCopilotRegistry(ctx, nil, registryTarget(), nil, false)
		if err == nil || !strings.Contains(err.Error(), "session token") {
			t.Fatalf("err = %v, want a session-token error", err)
		}
		if *minted != 1 || len(cr.Argv) != 0 || len(cr.Env) != 0 || len(*revoked) != 0 {
			t.Errorf("minted=%d revoked=%v launch=%+v; want an empty launch and nothing to revoke", *minted, *revoked, cr)
		}
	})

	t.Run("write failure revokes the token", func(t *testing.T) {
		_, revoked := registryHarness(t, func() (string, error) { return "tok-w", nil })
		rt := os.Getenv("XDG_RUNTIME_DIR")
		if err := os.WriteFile(filepath.Join(rt, "claude-code"), nil, 0o600); err != nil { // a file where the dir must go
			t.Fatal(err)
		}
		cr, err := ConfigureCopilotRegistry(ctx, nil, registryTarget(), nil, false)
		if err == nil || len(cr.Argv) != 0 {
			t.Fatalf("launch = %+v, err = %v; want an error and no argv", cr, err)
		}
		if !slices.Equal(*revoked, []string{"tok-w"}) {
			t.Errorf("revoked = %v, want [tok-w]", *revoked)
		}
	})

	t.Run("render failure returns the token to revoke", func(t *testing.T) {
		registryHarness(t, func() (string, error) { return "tok-r", nil })
		_, tok, err := prepareRegistry(ctx, copilotreg.Registry{}, "dbx", 1)
		if err == nil || !strings.Contains(err.Error(), "render registry") || tok != "tok-r" {
			t.Errorf("token = %q, err = %v; want the minted token and a render error", tok, err)
		}
	})

	t.Run("unregistered model", func(t *testing.T) {
		minted, _ := registryHarness(t, func() (string, error) { return "tok-u", nil })
		cr, err := ConfigureCopilotRegistry(ctx, nil, registryTarget(), []string{"--model", "no-such-model"}, false)
		if !errors.Is(err, copilotreg.ErrUnknownModel) || len(cr.Argv) != 0 {
			t.Fatalf("launch = %+v, err = %v; want ErrUnknownModel and no argv", cr, err)
		}
		if *minted != 0 {
			t.Errorf("minted %d tokens for a launch that was refused before any mint", *minted)
		}
	})
}

// A good launch: the registry file is the child's, inherited GitHub/BYOK routing is on the Unset list, --model is
// consumed into Argv, the token is only in the 0600 file, and Revoke (the OnExecFail hook) drops exactly that token.
func TestConfigureCopilotRegistryLaunchEnv(t *testing.T) {
	_, revoked := registryHarness(t, func() (string, error) { return "tok-ok", nil })
	cr, err := ConfigureCopilotRegistry(context.Background(), nil, registryTarget(),
		[]string{"--model", "databricks-claude-sonnet-5-5", "-p", "hi"}, false)
	if err != nil {
		t.Fatal(err)
	}
	file := ""
	for _, kv := range cr.Env {
		if v, ok := strings.CutPrefix(kv, "COPILOT_PROVIDERS_CONFIG="); ok {
			file = v
		}
		if strings.Contains(kv, "tok-ok") {
			t.Errorf("session token in env: %q", kv)
		}
	}
	if raw, err := os.ReadFile(file); err != nil || !strings.Contains(string(raw), "tok-ok") {
		t.Fatalf("registry file %q = %q, %v; want the token inside", file, raw, err)
	}
	if !slices.Contains(cr.Env, "COPILOT_OFFLINE=true") {
		t.Errorf("env %v lacks COPILOT_OFFLINE=true", cr.Env)
	}
	for _, n := range []string{"GH_TOKEN", "GITHUB_TOKEN", "COPILOT_GITHUB_TOKEN", "COPILOT_PROVIDERS_CONFIG", "COPILOT_MODEL"} {
		if !slices.Contains(cr.Unset, n) {
			t.Errorf("Unset lacks %s", n)
		}
	}
	if len(cr.Argv) < 2 || cr.Argv[0] != "--model" || !strings.HasSuffix(cr.Argv[1], "/claude-sonnet-5-5") ||
		!slices.Equal(cr.Args, []string{"-p", "hi"}) {
		t.Errorf("argv = %q, args = %q; want the registry ref consumed from --model", cr.Argv, cr.Args)
	}
	cr.Revoke()
	if !slices.Equal(*revoked, []string{"tok-ok"}) {
		t.Errorf("revoked = %v, want [tok-ok]", *revoked)
	}
	(copilotRegistry{}).Revoke() // a launch that never minted revokes nothing
	if len(*revoked) != 1 {
		t.Errorf("empty-token Revoke hit the router: %v", *revoked)
	}
}
