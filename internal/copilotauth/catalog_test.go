package copilotauth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func seal(t *testing.T, s Snapshot[Model]) {
	t.Helper()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	err = vault.UpdateSealed(func(tx *vault.Tx, key *vault.Key) error {
		return tx.PutCache(key, AccountRecord(s.Account), string(data), "github/copilot", s.DiscoveredAt)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A family is resolved against the snapshot of the row picked for the run, not the pinned row's account.
func TestIDsFollowTheSelectedRow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake gh is a shell script")
	}
	hosttest.PinScratch(t)
	vaulttest.UseKeyring(t)
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	t.Setenv("COPILOT_HOME", t.TempDir())
	for _, key := range TokenEnvs {
		t.Setenv(key, "")
	}
	bin := t.TempDir()
	write(t, filepath.Join(bin, "gh"), "#!/bin/sh\necho \"tok-$4\"\n")
	if err := os.Chmod(filepath.Join(bin, "gh"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	row := `{"protocol":"openai","base_url":"https://api.githubcopilot.com","login":"%s","auth":{"type":"cli","login":"copilot"}}`
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"github":{"account-a":`+fmt.Sprintf(row, "account-a")+
		`,"account-b":`+fmt.Sprintf(row, "account-b")+`}}}`)
	vaulttest.SetRecord(t, "copilot", "profile", "account-a")
	write(t, filepath.Join(os.Getenv("GH_CONFIG_DIR"), "hosts.yml"),
		"github.com:\n    users:\n        account-a:\n            oauth_token: x\n        account-b:\n            oauth_token: x\n    user: account-a\n")
	seal(t, Snapshot[Model]{Schema: 1, Account: "account-b", Source: SrcPinned, DiscoveredAt: time.Now(),
		TokenHash: textutil.SHA256Hex([]byte("tok-account-b")), Models: []Model{{ID: "gpt-6.2-sol", Enabled: true}}})
	Reset()
	t.Cleanup(Reset)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := IDs(reg.Providers["account-b"]); !slices.Equal(got, []string{"gpt-6.2-sol"}) {
		t.Errorf("account-b ids = %v, want its own snapshot", got)
	}
	if got := IDs(reg.Providers["account-a"]); got != nil {
		t.Errorf("pinned account-a ids = %v, want none: the snapshot is account-b's", got)
	}
	seal(t, Snapshot[Model]{Schema: 1, Account: "account-a", Source: SrcPinned, DiscoveredAt: time.Now(),
		TokenHash: textutil.SHA256Hex([]byte("tok-account-a")), Models: []Model{{ID: "kimi-k3", Enabled: true}}})
	Reset()
	if got := IDs(reg.Providers["account-a"]); !slices.Equal(got, []string{"kimi-k3"}) {
		t.Errorf("account-a ids after the reset = %v, want its new listing", got)
	}
	if got := IDs(reg.Providers["account-b"]); !slices.Equal(got, []string{"gpt-6.2-sol"}) {
		t.Errorf("account-b ids after account-a's listing = %v, want its own snapshot kept", got)
	}
}

// loginScratch is a hermetic vault and fake gh, with gh's active account and copilot's stored login when non-empty.
func loginScratch(t *testing.T, gh, copilot string) {
	t.Helper()
	hosttest.PinScratch(t)
	vaulttest.UseKeyring(t)
	ghDir, cpDir, bin := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("GH_CONFIG_DIR", ghDir)
	t.Setenv("COPILOT_HOME", cpDir)
	for _, key := range TokenEnvs {
		t.Setenv(key, "")
	}
	write(t, filepath.Join(bin, "gh"), "#!/bin/sh\necho tok\n")
	if err := os.Chmod(filepath.Join(bin, "gh"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if gh != "" {
		write(t, filepath.Join(ghDir, "hosts.yml"), fmt.Sprintf(
			"github.com:\n    users:\n        %[1]s:\n            oauth_token: x\n    user: %[1]s\n", gh))
	}
	if copilot != "" {
		write(t, filepath.Join(cpDir, "config.json"), `{"lastLoggedInUser":{"login":"`+copilot+`"}}`)
	}
}

// A pin is used as is; unpinned, copilot's own login beats the vault token, which beats gh's active account.
func TestAuthSourceForOrder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake gh is a shell script")
	}
	cases := []struct {
		name, pin, gh, copilot, envTok string
		vaultTok                       bool
		want                           Source
		account                        string
	}{
		{name: "pinned", pin: "pin_user", gh: "pin_user", copilot: "cp_user", vaultTok: true, want: SrcPinned, account: "pin_user"},
		{name: "copilot login", gh: "gh_user", copilot: "cp_user", vaultTok: true, want: SrcCopilot, account: "cp_user"},
		{name: "vault token", vaultTok: true, want: SrcStoredToken},
		{name: "gh active", gh: "gh_user", want: SrcActive, account: "gh_user"},
		{name: "env shadows copilot login", copilot: "cp_user", envTok: "GH_TOKEN", want: "GH_TOKEN"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			loginScratch(t, c.gh, c.copilot)
			if c.vaultTok {
				vaulttest.Secret(t, providers.CopilotTokenEnv, "ghp_stored")
			}
			if c.envTok != "" {
				t.Setenv(c.envTok, "ghp_env")
			}
			a, ok := AuthSourceFor(c.pin)
			if !ok || a.Source != c.want || a.Account != c.account {
				t.Errorf("AuthSourceFor(%q) = %s/%s ok=%v, want %s/%s", c.pin, a.Account, a.Source, ok, c.account, c.want)
			}
		})
	}
}
