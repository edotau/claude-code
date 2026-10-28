package copilotsdk

import (
	"encoding/base64"
	"os"
	"testing"
)

// TestMain points every test at a throwaway config dir and vault key, so none can touch the live config.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "copilotsdk-test-")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("CLAUDE_CONFIG_DIR", dir)
	_ = os.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
