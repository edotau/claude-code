package doctor

import (
	"strings"
	"testing"
	"time"
)

func TestContractCheckEmpty(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	row := contractCheck(time.Now(), 0, nil)
	if !row.OK || !strings.Contains(row.Detail, "none") {
		t.Fatalf("row = %+v; want ok and none", row)
	}
}
