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

	"github.com/edotau/claude-code/internal/dev/github"
	"github.com/edotau/claude-code/internal/platform/hosttest"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/vault/vaulttest"
	"github.com/edotau/claude-code/internal/routing/models"
	"github.com/edotau/claude-code/internal/routing/providers"
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

// The copilot account row only reads; it names copilot's account, never gh's active one.
func TestCopilotAccountLineIsReadOnly(t *testing.T) {
	hosttest.GithubScratch(t)
	vaulttest.UseKeyring(t)
	reg := &providers.Registry{Providers: map[string]*providers.Provider{"work": {Provider: "github", Login: "wl"}}}
	var out bytes.Buffer
	copilotAccountLine(&dialog{reg: reg, out: &out})
	if got := out.String(); !strings.HasPrefix(got, "copilot account ") || strings.Contains(got, "gh active") {
		t.Errorf("output = %q, want one copilot account row", got)
	}
}

// Unpinned, the row names copilot's own stored login, the first source an unpinned dispatch uses.
func TestCopilotAccountLineNamesUnpinnedSource(t *testing.T) {
	menuTestEnv(t)
	copilotLoggedInAs(t, "xuser")
	var out bytes.Buffer
	copilotAccountLine(&dialog{out: &out})
	if got := out.String(); !strings.Contains(got, "xuser (copilot login)") || strings.Contains(got, "gh's active") {
		t.Errorf("output = %q, want xuser (copilot login)", got)
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
	// GH_TOKEN is not copilot's own login
	if !regexp.MustCompile(`copilot CLI +ABSENT`).MatchString(got) {
		t.Errorf("want copilot CLI absent with only GH_TOKEN:\n%s", got)
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

// fakeGHActive makes yuser gh's active account, with a fake gh that serves a token.
func fakeGHActive(t *testing.T) {
	t.Helper()
	hosttest.SkipOnWindows(t, "fake gh is a sh script")
	ghDir := t.TempDir()
	t.Setenv("GH_CONFIG_DIR", ghDir)
	hosts := "github.com:\n    users:\n        yuser:\n    user: yuser\n"
	gh := "#!/bin/sh\n[ \"$1 $2\" = \"auth token\" ] && echo fixture-token && exit 0\nexit 1\n"
	if err := os.WriteFile(filepath.Join(ghDir, "hosts.yml"), []byte(hosts), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("PATH"), "gh"), []byte(gh), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := github.GHActiveAccount(); got != "yuser" {
		t.Fatalf("fixture: gh active = %q, want yuser", got)
	}
}

// gh's active account is not copilot's login: the copilot CLI row is present only on copilot's own stored login.
func TestGithubStatusCopilotRowIsCopilotsOwnLogin(t *testing.T) {
	reg := menuTestEnv(t)
	fakeGHActive(t)
	for _, login := range []string{"", "xuser"} {
		copilotLoggedInAs(t, login)
		var out bytes.Buffer
		ghOK, copilotOK, _ := githubStatusRows(&dialog{reg: reg, out: &out})
		present := strings.Contains(out.String(), "copilot CLI      present (logged in as xuser)")
		if !ghOK || copilotOK != (login != "") || present != copilotOK {
			t.Errorf("copilot login %q: gh=%v copilot=%v out=%q", login, ghOK, copilotOK, out.String())
		}
	}
}
