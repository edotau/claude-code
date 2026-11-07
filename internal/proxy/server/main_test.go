package server

import (
	"os"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/proxy"
)

// TestMain runs hermetically (hosttest) and treats this test binary as the proxy process.
func TestMain(m *testing.M) {
	proxy.StubHolderCommForTest()
	os.Exit(hosttest.Main(m))
}
