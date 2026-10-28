package proxy

import (
	"encoding/base64"
	"os"
	"testing"
)

// TestMain points every test at a throwaway config dir and vault key, so none can touch the live vault or secret.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "proxy-test-")
	if err != nil {
		panic(err)
	}
	os.Setenv("CLAUDE_CONFIG_DIR", dir)
	os.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	os.Setenv("CLAUDE_PROFILE", "")
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
