package databricks

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

func TestImportSealedPATIsFoundByCredentialForUnderscoreSuffixID(t *testing.T) {
	useVaultTest(t)
	cfg := filepath.Join(t.TempDir(), "cfg")
	body := "[dev_]\nhost = https://example.cloud.databricks.com\ntoken = dapi-e2e-value\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--file", cfg, "--apply", "--seal-secrets"}
	if code, out, errs := captureOutput(t, func() int { return importCfg(args) }); code != 0 {
		t.Fatalf("import: %d %q %q", code, out, errs)
	}
	reg, err := providers.Load()
	if err != nil || reg.Providers["dev_"] == nil {
		t.Fatalf("row dev_ missing: %v", err)
	}
	got, err := providers.Credential(context.Background(), reg.Providers["dev_"])
	if err != nil || got != "dapi-e2e-value" {
		t.Fatalf("Credential = %q, %v", got, err)
	}
}
