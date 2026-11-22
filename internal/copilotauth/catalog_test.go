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
		return tx.PutSealed(key, AccountRecord(s.Account), string(data), "github/copilot", s.DiscoveredAt)
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
