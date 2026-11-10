package server

import (
	"os"
	"testing"

	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/hosttest"
)

// TestMain runs hermetically (hosttest) and treats this test binary as the proxy process.
func TestMain(m *testing.M) {
	gateway.HolderComm = func(int) string { return "claude-code" }
	os.Exit(hosttest.Main(m))
}
