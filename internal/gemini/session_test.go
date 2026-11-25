package gemini

import (
	"context"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Route tokens must survive launcher exit without granting access to any other route.
func TestAgyGatewayRouteTokensSurviveLauncherExit(t *testing.T) {
	t.Cleanup(agent.StubRouter("http://127.0.0.1:19876"))
	agent.ResetSessionTokens()
	t.Cleanup(agent.ResetSessionTokens)
	helper := agySessionSleeper(t)
	start := agySessionStart(t, helper.Process.Pid)
	const secret = "agy-route-test-secret"
	now := time.Now()
	orig := agent.MintSession
	agent.MintSession = func(ctx context.Context, profiles []string, _ int) (string, error) {
		fence, open, err := proxy.ConfinementFence(ctx)
		if err != nil {
			return "", err
		}
		return proxy.SignSessionToken(secret, proxy.SessionClaims{
			Profiles: profiles, PID: helper.Process.Pid, Start: start, Fence: fence, Open: open,
		}, now)
	}
	t.Cleanup(func() { agent.MintSession = orig })
	tokens := map[string]string{}
	for _, profile := range []string{"dev", "qa", "prod"} {
		tokens[profile] = agyRouteToken(t, secret, profile, now)
	}
	_ = helper.Process.Kill()
	_ = helper.Wait()
	for profile, tok := range tokens {
		for _, route := range []string{"dev", "qa", "prod", "databricks-dev"} {
			err := proxy.CheckSessionToken(secret, tok, route, now, proc.StartTime)
			agySessionEqual(t, err == nil, route == profile)
		}
	}
}

func agyRouteToken(t *testing.T, secret, profile string, now time.Time) string {
	t.Helper()
	ctx := proxy.WithFence(context.Background(), []string{profile}, false)
	p := &providers.Provider{Name: profile, Preset: providers.PresetDatabricks, Auth: providers.Auth{Type: "pat"}}
	env, _, err := geminiGateway(ctx, p, "gemini-3.8-flash", &agent.Headless{})
	if err != nil {
		t.Fatal(err)
	}
	values := proc.ValuesFromEnv(env)
	agySessionEqual(t, values["AGY_LLM_GATEWAY_URL"], "http://127.0.0.1:19876/p/"+profile+"/gemini")
	tok, err := rebindAgyToken(secret, values["AGY_LLM_GATEWAY_API_KEY"], os.Getpid(), now)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := proxy.ParseSessionToken(secret, tok)
	if err != nil {
		t.Fatal(err)
	}
	agySessionEqual(t, claims.PID, os.Getpid())
	agySessionEqual(t, claims.Profiles, []string{profile})
	agySessionEqual(t, claims.Fence, []string{profile})
	agySessionEqual(t, claims.Open, false)
	agySessionEqual(t, claims.Failover, "")
	return tok
}

// agySessionSleeper supplies a real process binding without relying on fixed /bin paths.
func agySessionSleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("native Windows does not support proxy process bindings or same-PID exec")
	}
	bin, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep is unavailable")
	}
	cmd := exec.Command(bin, "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd
}

func agySessionStart(t *testing.T, pid int) string {
	t.Helper()
	start, err := proc.StartTime(pid)
	if err != nil {
		t.Fatal(err)
	}
	return start
}

func agySessionEqual(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func launchTokenFor(t *testing.T, secret string, pid int, start string, now time.Time) string {
	t.Helper()
	tok, err := proxy.SignSessionToken(secret, proxy.SessionClaims{Profiles: []string{"dev"}, PID: pid, Start: start, Open: true}, now)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestAgySessionBootstrapEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native Windows has no same-PID exec")
	}
	vaulttest.Use(t)
	secret, err := proxy.ClientSecret()
	if err != nil {
		t.Fatal(err)
	}
	tok := launchTokenFor(t, secret, os.Getpid(), agySessionStart(t, os.Getpid()), time.Now())
	key := vault.EnvKey()
	t.Setenv("HARNESS_VAULT_KEY", "") // main adopts this key before any launch; the live env is empty
	oldKey := agyEnvKey
	t.Cleanup(func() { agyEnvKey = oldKey })
	agyEnvKey = func() string { return key } // model the adopted key without mutating vault package globals
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "arg with spaces")
	cmd.Env = []string{"AGY_LLM_GATEWAY_URL=http://127.0.0.1:19876/p/dev/gemini", "AGY_LLM_GATEWAY_API_KEY=" + tok, "KEEP_ME=kept"}
	if err := bindAgyChild(cmd); err != nil {
		t.Fatal(err)
	}
	agySessionEqual(t, cmd.Args, []string{self, agyExecArg, self, "arg with spaces"})
	env := proc.ValuesFromEnv(cmd.Env)
	agySessionEqual(t, env["HARNESS_VAULT_KEY"], key)
	agySessionEqual(t, env["HARNESS_VAULT_KEY"] != "", true)
	for k, v := range env {
		t.Setenv(k, v)
	}
	agyCheckVendorExec(t, secret, tok)
}

func agyCheckVendorExec(t *testing.T, secret, original string) {
	t.Helper()
	oldExec := execAgyImage
	t.Cleanup(func() { execAgyImage = oldExec })
	called := false
	execAgyImage = func(bin string, argv, environ []string) error {
		called = true
		final := proc.ValuesFromEnv(environ)
		_, hasKey := final["HARNESS_VAULT_KEY"]
		agySessionEqual(t, hasKey, false)
		agySessionEqual(t, bin, "vendor")
		agySessionEqual(t, argv, []string{"arg with spaces"})
		agySessionEqual(t, final["KEEP_ME"], "kept")
		agySessionEqual(t, final["AGY_LLM_GATEWAY_API_KEY"] != original, true)
		if err := proxy.CheckSessionToken(secret, final["AGY_LLM_GATEWAY_API_KEY"], "dev", time.Now(), proc.StartTime); err != nil {
			t.Error(err)
		}
		return nil
	}
	if err := execAgy("vendor", []string{"arg with spaces"}); err != nil {
		t.Fatal(err)
	}
	agySessionEqual(t, called, true)
}

func TestAgySessionLoginAndLookupError(t *testing.T) {
	login := exec.Command(os.Args[0], "login")
	if err := bindAgyChild(login); err != nil {
		t.Fatal(err)
	}
	agySessionEqual(t, login.Args, []string{os.Args[0], "login"})
	bad := exec.Command(os.Args[0])
	bad.Err = exec.ErrNotFound
	agySessionEqual(t, bindAgyChild(bad), exec.ErrNotFound)
}

func TestAgySessionRebindPreservesClaims(t *testing.T) {
	agy := agySessionSleeper(t)
	start := agySessionStart(t, os.Getpid())
	const secret = "agy-claims-test"
	now := time.Now()
	for _, c := range []proxy.SessionClaims{
		{Open: true}, {}, {Fence: []string{"dev", "qa"}, Failover: "databricks"},
	} {
		c.Profiles, c.PID, c.Start = []string{"dev"}, os.Getpid(), start
		tok, err := proxy.SignSessionToken(secret, c, now)
		if err != nil {
			t.Fatal(err)
		}
		rebound, err := rebindAgyToken(secret, tok, agy.Process.Pid, now)
		if err != nil {
			t.Fatal(err)
		}
		got, err := proxy.ParseSessionToken(secret, rebound)
		if err != nil {
			t.Fatal(err)
		}
		agySessionEqual(t, got.Fence, c.Fence)
		agySessionEqual(t, got.Open, c.Open)
		agySessionEqual(t, got.Failover, c.Failover)
		agySessionEqual(t, got.Profiles, c.Profiles)
		if err := proxy.CheckSessionToken(secret, rebound, "dev", now, proc.StartTime); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAgySessionRefusesInvalidAndExitedBindings(t *testing.T) {
	agy := agySessionSleeper(t)
	const secret = "agy-death-test"
	now := time.Now()
	tok := launchTokenFor(t, secret, os.Getpid(), agySessionStart(t, os.Getpid()), now)
	_, err := rebindAgyToken("wrong-secret", tok, os.Getpid(), now)
	agySessionEqual(t, err != nil, true)
	_, err = rebindAgyToken(secret, tok, os.Getpid(), now.Add(proxy.SessionTokenTTL+time.Second))
	agySessionEqual(t, err != nil, true)
	rebound, err := rebindAgyToken(secret, tok, agy.Process.Pid, now)
	if err != nil {
		t.Fatal(err)
	}
	_ = agy.Process.Kill()
	_ = agy.Wait()
	err = proxy.CheckSessionToken(secret, rebound, "dev", now, proc.StartTime)
	agySessionEqual(t, err != nil, true)
	_, err = rebindAgyToken(secret, rebound, os.Getpid(), now)
	agySessionEqual(t, err != nil, true)
}
