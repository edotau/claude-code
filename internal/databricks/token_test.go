package databricks

import (
	"os"
	"strings"
	"testing"
)

func TestMintProxyTokenRejectsInvalidConfigDir(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var token string
	code, stdout, stderr := runCaptured(t, func() int {
		var code int
		token, code = MintProxyToken("dev", "invalid\x00dir")
		return code
	})
	if code != 1 || token != "" || stdout != "" || !strings.Contains(stderr, "gateway token:") {
		t.Fatalf("exit %d token %q stdout %q stderr %q", code, token, stdout, stderr)
	}
	if strings.ContainsRune(os.Getenv("CLAUDE_CONFIG_DIR"), '\x00') {
		t.Fatal("invalid config dir was applied")
	}
}
