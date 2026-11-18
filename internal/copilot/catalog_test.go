package copilot

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func BenchmarkCopilotUsable(b *testing.B) {
	for _, ready := range []bool{false, true} {
		name := "unavailable"
		if ready {
			name = "ready"
		}
		b.Run(name, func(b *testing.B) {
			infos := make([]providers.ModelInfo, 64)
			for i := range infos {
				infos[i] = providers.ModelInfo{ID: "model", Task: "llm/v1/chat"}
				if ready {
					infos[i].Ready = "READY"
				}
			}
			b.ReportAllocs()
			for b.Loop() {
				if copilotUsable(infos) != ready {
					b.Fatal("unexpected catalog usability")
				}
			}
		})
	}
}

// A family is resolved against the snapshot of the row picked for the run, not the pinned row's account.
func TestSnapshotIDsFollowTheSelectedRow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake gh is a shell script")
	}
	catalogScratch(t)
	bin := t.TempDir()
	regressionWrite(t, filepath.Join(bin, "gh"), "#!/bin/sh\necho \"tok-$4\"\n")
	if err := os.Chmod(filepath.Join(bin, "gh"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	row := `{"protocol":"openai","base_url":"https://api.githubcopilot.com","login":"%s","auth":{"type":"cli","login":"copilot"}}`
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"github":{"account-a":`+fmt.Sprintf(row, "account-a")+
		`,"account-b":`+fmt.Sprintf(row, "account-b")+`}}}`)
	vaulttest.SetRecord(t, "copilot", "profile", "account-a")
	regressionWrite(t, filepath.Join(os.Getenv("GH_CONFIG_DIR"), "hosts.yml"),
		"github.com:\n    users:\n        account-a:\n            oauth_token: x\n        account-b:\n            oauth_token: x\n    user: account-a\n")
	if err := saveCatalogSnapshot(catalogSnapshot{Schema: 1, Account: "account-b", Source: srcPinned, DiscoveredAt: time.Now(),
		TokenHash: textutil.SHA256Hex([]byte("tok-account-b")), Models: []SDKModel{{ID: "gpt-6.2-sol", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	snapshotMap = map[string][]string{}
	t.Cleanup(func() { snapshotMap = map[string][]string{} })
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshotIDs(reg.Providers["account-b"]); !slices.Equal(got, []string{"gpt-6.2-sol"}) {
		t.Errorf("account-b ids = %v, want its own snapshot", got)
	}
	if got := snapshotIDs(reg.Providers["account-a"]); got != nil {
		t.Errorf("pinned account-a ids = %v, want none: the snapshot is account-b's", got)
	}
}
