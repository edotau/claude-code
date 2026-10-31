package agent

import (
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

func testTable(t *testing.T) *router.RouteTable {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // scratch vault: never the real pins or logins
	reg := &providers.Registry{Routing: providers.Routing{Opencode: providers.OpencodeRouting{FreeTiers: &[]string{"sonnet", "haiku"}}}}
	tb, err := router.LoadFrom(reg, "work-user")
	if err != nil {
		t.Fatal(err)
	}
	return tb
}

func allConfigured(string, router.Cell) error { return nil }

var now0 = time.Date(2026, 10, 25, 2, 6, 0, 0, time.Local)
