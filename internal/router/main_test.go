package router

import (
	"encoding/base64"
	"os"
	"testing"
)

// TestMain points the config dir and vault key at a throwaway dir: a test must never read the developer's vault pins.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "router-test-")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("CLAUDE_CONFIG_DIR", dir)
	_ = os.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
