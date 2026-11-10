package menu

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
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
