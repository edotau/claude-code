package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// RunOwnLogin: no inherited keep exception, harness secret or unregistered credential reaches the vendor.
func TestRunOwnLoginScrubsEveryCredential(t *testing.T) {
	vaulttest.Use(t)
	out := filepath.Join(t.TempDir(), "env")
	bin := pathstest.FakeVendor(t, "codex", `env > `+out)
	t.Setenv("HARNESS_KEEP_ENV", "HARNESS_VAULT_KEY,DATABRICKS_TOKEN,MISTRAL_API_KEY")
	for _, kv := range []string{"HARNESS_VAULT_KEY=k", "DATABRICKS_TOKEN=d", "MISTRAL_API_KEY=m", "HOME_KEEP=1"} {
		name, v, _ := strings.Cut(kv, "=")
		t.Setenv(name, v)
	}
	if rc := RunOwnLogin(t.Context(), "codex", bin, nil, nil); rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, gone := range []string{"HARNESS_VAULT_KEY=", "DATABRICKS_TOKEN=", "MISTRAL_API_KEY="} {
		if strings.Contains(got, gone) {
			t.Errorf("%s reached the vendor process", gone)
		}
	}
	if !strings.Contains(got, "HOME_KEEP=1") {
		t.Error("a plain env name was dropped")
	}
}
