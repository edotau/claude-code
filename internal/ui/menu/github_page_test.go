package menu

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
	sdk "github.com/github/copilot-sdk/go"
)

func TestPickCopilotModelsMarksChangedAndReloadsRegistry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("catalog fixture requires a POSIX shell")
	}
	reg := menuTestEnv(t)
	fakeCopilotBin(t)
	script := fmt.Sprintf(`#!/bin/sh
while IFS= read -r line; do
  case "$line" in Content-Length:*) n=${line#*: }; n=${n%%?};; esac
  [ -z "${line%%?}" ] || continue
  body=$(/usr/bin/head -c "$n")
  id=$(printf '%%s' "$body" | /usr/bin/sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  r='{"jsonrpc":"2.0","id":"'"$id"'","result":{"protocolVersion":%d,"models":[{"id":"catalog-model","name":"Catalog Model"}]}}'
  printf 'Content-Length: %%s\r\n\r\n%%s' "${#r}" "$r"
done
`, sdk.GetSDKProtocolVersion())
	if err := os.WriteFile(filepath.Join(paths.BinDir(), "copilot"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	in := strings.NewReader("2\n2\n2\n")
	var out bytes.Buffer
	d := &dialog{reg: reg, o: Options{In: in}, br: bufio.NewReader(in), out: &out}
	pickCopilotModels(d)
	if !d.changed {
		t.Fatalf("saved fallback models must trigger config regeneration: %s", &out)
	}
	if d.reg == reg {
		t.Fatal("saved fallback models must reload the dialog registry")
	}
	for _, alias := range []string{models.Opus, models.Sonnet, models.Haiku} {
		if got := d.reg.Providers["personal"].Models[alias]; got != "catalog-model" {
			t.Errorf("reloaded %s = %q, want catalog-model", alias, got)
		}
	}
}

func TestGithubAuthLineFlagsPinnedProfileWithoutLogin(t *testing.T) {
	githubScratch(t)
	vaulttest.UseKeyring(t)
	if err := vault.SetGitHubAuth("work"); err != nil {
		t.Fatal(err)
	}
	reg := &providers.Registry{Providers: map[string]*providers.Provider{"work": {Provider: "github"}}}
	got := githubAuthLine(&dialog{reg: reg})
	if !strings.HasPrefix(got, "work") || !strings.Contains(got, "drift") {
		t.Errorf("githubAuthLine = %q, want the work pin flagged as drift", got)
	}
}

func TestGithubAuthLineNamesDanglingPin(t *testing.T) {
	githubScratch(t)
	vaulttest.UseKeyring(t)
	if err := vault.SetGitHubAuth("gone"); err != nil {
		t.Fatal(err)
	}
	reg := &providers.Registry{Providers: map[string]*providers.Provider{"work": {Provider: "github"}}}
	got := githubAuthLine(&dialog{reg: reg})
	if !strings.Contains(got, "pin names no github profile") {
		t.Errorf("githubAuthLine = %q, want the dangling pin named as such", got)
	}
}

func TestPickGitHubAuthRollsGHBackWhenVaultWriteFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake gh requires a POSIX shell")
	}
	githubScratch(t)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\necho tok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if err := os.MkdirAll(vault.Path(), 0o700); err != nil { // a directory at the db path makes every vault write fail
		t.Fatal(err)
	}
	ghDir := t.TempDir()
	t.Setenv("GH_CONFIG_DIR", ghDir)
	hosts := "github.com:\n    user: old\n    users:\n        old:\n"
	if err := os.WriteFile(filepath.Join(ghDir, "hosts.yml"), []byte(hosts), 0o600); err != nil {
		t.Fatal(err)
	}
	var switched []string
	orig := copilot.SwitchGH
	copilot.SwitchGH = func(login string) error { switched = append(switched, login); return nil }
	t.Cleanup(func() { copilot.SwitchGH = orig })
	reg := &providers.Registry{Providers: map[string]*providers.Provider{"work": {Provider: "github", Login: "new"}}}
	var out bytes.Buffer
	d := &dialog{reg: reg, out: &out, br: bufio.NewReader(strings.NewReader("2\n"))}
	pickGitHubAuth(d)
	if got := strings.Join(switched, ","); got != "new,old" {
		t.Errorf("SwitchGH calls = %q, want new,old (switch then rollback); output:\n%s", got, out.String())
	}
}

func authFlagDialog(t *testing.T, auth string, o Options) (*dialog, *[]string, *bytes.Buffer) {
	t.Helper()
	githubScratch(t)
	vaulttest.UseKeyring(t)
	var switched []string
	orig := copilot.SwitchGH
	copilot.SwitchGH = func(login string) error { switched = append(switched, login); return nil }
	t.Cleanup(func() { copilot.SwitchGH = orig })
	reg := &providers.Registry{Providers: map[string]*providers.Provider{
		"work": {Provider: "github", Login: "wl"}, "follow": {Provider: "github", Login: "fl"}}}
	o.Auth, o.AuthSet = auth, true
	var out bytes.Buffer
	return &dialog{reg: reg, out: &out, o: o}, &switched, &out
}

func TestRunGithubAuthFlagPinsAndSwitches(t *testing.T) {
	d, switched, out := authFlagDialog(t, "work", Options{})
	if !runGithubAuthFlag(d) || vault.GitHubAuth() != "work" || strings.Join(*switched, ",") != "wl" {
		t.Errorf("pin=%q switched=%v out=%s", vault.GitHubAuth(), *switched, out)
	}
}

func TestRunGithubAuthFlagFollowClearsPinWithoutSwitch(t *testing.T) {
	d, switched, _ := authFlagDialog(t, "", Options{})
	d.reg.Providers["follow"].Login = ""
	if err := vault.SetGitHubAuth("work"); err != nil {
		t.Fatal(err)
	}
	d.o.Auth = "follow"
	if !runGithubAuthFlag(d) || vault.GitHubAuth() != "" || len(*switched) != 0 {
		t.Errorf("pin=%q switched=%v", vault.GitHubAuth(), *switched)
	}
}

func TestRunGithubAuthFlagProfileNamedFollowIsPinned(t *testing.T) {
	d, switched, _ := authFlagDialog(t, "follow", Options{})
	if !runGithubAuthFlag(d) || vault.GitHubAuth() != "follow" || strings.Join(*switched, ",") != "fl" {
		t.Errorf("pin=%q switched=%v", vault.GitHubAuth(), *switched)
	}
}

func TestRunGithubAuthFlagUnknownAndDryRun(t *testing.T) {
	d, _, out := authFlagDialog(t, "nope", Options{})
	if runGithubAuthFlag(d) || !strings.Contains(out.String(), "valid: follow") {
		t.Errorf("unknown name must fail listing valid names: %s", out)
	}
	d, switched, _ := authFlagDialog(t, "work", Options{DryRun: true})
	if !runGithubAuthFlag(d) || vault.GitHubAuth() != "" || len(*switched) != 0 {
		t.Errorf("dry-run touched state: pin=%q switched=%v", vault.GitHubAuth(), *switched)
	}
}

// githubScratch isolates CLAUDE_CONFIG_DIR, HOME, XDG, PATH and every credential env var github.go
// reads, so a test never touches a real gh/copilot login or a real vault overlay.
func githubScratch(t *testing.T) {
	t.Helper()
	cfg, home, xdg := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	for _, k := range []string{"ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GH_TOKEN", "GITHUB_TOKEN", "COPILOT_GITHUB_TOKEN"} {
		t.Setenv(k, "")
	}
}

func TestRunGithubLoginPrintsVendorBinError(t *testing.T) {
	githubScratch(t)
	_, err := paths.AgentBin("copilot")
	if err == nil {
		t.Fatal("expected missing vendor")
	}
	var out bytes.Buffer
	runGithubLogin(&dialog{out: &out}, []string{"copilot", "login"}, "copilot")
	if want := "copilot: " + err.Error() + "\n"; out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

// The piped `configure github` report prints only booleans, provider/model names and the fix commands —
// never a token-shaped string, even when a real-looking GitHub token sits in the environment.
func TestGithubSectionNonTTYOutputNamesNoSecret(t *testing.T) {
	githubScratch(t)
	t.Setenv("GH_TOKEN", "ghp_FAKEtokenlookingvalue1234567890")
	var out bytes.Buffer
	_ = Run(Options{Check: true, In: strings.NewReader(""), Out: &out}, "github")
	got := out.String()
	if strings.Contains(got, "ghp_FAKEtokenlookingvalue1234567890") {
		t.Errorf("github section leaked a credential:\n%s", got)
	}
	if !regexp.MustCompile(`copilot CLI +present`).MatchString(got) {
		t.Errorf("want copilot CLI to read present via GH_TOKEN:\n%s", got)
	}
	for _, want := range []string{"gh CLI", "copilot CLI", "route("} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}
