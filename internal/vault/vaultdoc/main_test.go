package vaultdoc

import (
	"os"
	"testing"

	_ "github.com/edotau/claude-code/internal/claude" // the vendors register the roster rows the vault tests list
	_ "github.com/edotau/claude-code/internal/codex"
	_ "github.com/edotau/claude-code/internal/copilot"
	_ "github.com/edotau/claude-code/internal/gemini"
	"github.com/edotau/claude-code/internal/hosttest"
	_ "github.com/edotau/claude-code/internal/opencode"
)

// TestMain runs in the hermetic host (hosttest): no live provider pins, vault or vendor CLIs are reachable.
func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }
