package menu

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/github"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
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
		if got := d.reg.Providers["copilot"].Models[alias]; got != "catalog-model" {
			t.Errorf("reloaded %s = %q, want catalog-model", alias, got)
		}
	}
}

func authFlagDialog(t *testing.T, auth string, o Options) (*dialog, *[]string, *bytes.Buffer) {
	t.Helper()
	hosttest.GithubScratch(t)
	vaulttest.UseKeyring(t)
	var switched []string
	orig := github.SwitchGH
	github.SwitchGH = func(login string) error { switched = append(switched, login); return nil }
	t.Cleanup(func() { github.SwitchGH = orig })
	reg := &providers.Registry{Providers: map[string]*providers.Provider{"work": {Provider: "github", Login: "wl"}}}
	o.Auth, o.AuthSet = auth, true
	var out bytes.Buffer
	return &dialog{reg: reg, out: &out, o: o}, &switched, &out
}

func TestRunGithubAuthFlagOnlySwitchesGH(t *testing.T) {
	d, switched, out := authFlagDialog(t, "wl", Options{})
	if !runGithubAuthFlag(d) || strings.Join(*switched, ",") != "wl" {
		t.Errorf("switched=%v out=%s", *switched, out)
	}
}

func TestRunGithubAuthFlagUnknownAndDryRun(t *testing.T) {
	d, switched, out := authFlagDialog(t, "nope", Options{})
	if runGithubAuthFlag(d) || !strings.Contains(out.String(), "valid:") || len(*switched) != 0 {
		t.Errorf("unknown name must fail listing valid names: %s", out)
	}
	d, switched, _ = authFlagDialog(t, "wl", Options{DryRun: true})
	if !runGithubAuthFlag(d) || len(*switched) != 0 {
		t.Errorf("dry-run switched: %v", *switched)
	}
}

func TestCopilotAccountLineIsReadOnly(t *testing.T) {
	d, switched, out := authFlagDialog(t, "", Options{})
	copilotAccountLine(d)
	if got := out.String(); !strings.HasPrefix(got, "copilot account ") || strings.Contains(got, "gh active") {
		t.Errorf("output = %q, want one copilot account row", got)
	}
	if len(*switched) != 0 {
		t.Errorf("status lines switched gh: %v", *switched)
	}
}

func TestSignInPrintsVendorBinError(t *testing.T) {
	hosttest.GithubScratch(t)
	_, err := paths.AgentBin("copilot")
	if err == nil {
		t.Fatal("expected missing vendor")
	}
	var out bytes.Buffer
	signIn(&dialog{out: &out}, "copilot", []string{"copilot", "login"})
	if want := "copilot: " + err.Error() + "\n"; out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

// The piped `configure github` report prints only booleans.
func TestGithubSectionNonTTYOutputNamesNoSecret(t *testing.T) {
	hosttest.GithubScratch(t)
	t.Setenv("GH_TOKEN", "rTyUiOpLkJhGfDsAzXcVb")
	var out bytes.Buffer
	_ = Run(Options{Check: true, In: strings.NewReader(""), Out: &out}, "github")
	got := out.String()
	if strings.Contains(got, "rTyUiOpLkJhGfDsAzXcVb") {
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

func TestGithubProfileNeverClaimsLoginlessRow(t *testing.T) {
	hosttest.GithubScratch(t)
	vaulttest.Use(t)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	if name, err := githubProfile(reg, "alice"); err != nil || name != "alice" {
		t.Fatalf("githubProfile = %q, %v; want alice, nil", name, err)
	}
	reg, err = providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	if reg.Providers["alice"] == nil || reg.Providers["copilot"] == nil {
		t.Errorf("want rows alice and copilot, got %v", reg.ProfilesOf("github"))
	}
}

// Owner decision 9 (2026-11-16): a login naming another provider's profile is refused, not rewritten into GitHub.
func TestGithubProfileRefusesNameClash(t *testing.T) {
	hosttest.GithubScratch(t)
	vaulttest.Use(t)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	if name, err := githubProfile(reg, "anthropic"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("githubProfile(anthropic) = %q, %v; want a name-clash refusal", name, err)
	}
	if reg, err = providers.Load(); err != nil {
		t.Fatal(err)
	}
	if p := reg.Providers["anthropic"]; p == nil || p.Login != "" || slices.Contains(reg.ProfilesOf("github"), "anthropic") {
		t.Errorf("anthropic row was rewritten: %+v", p)
	}
}
