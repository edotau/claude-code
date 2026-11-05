// Package agenttest stubs the agent engine's router so no test relaunches the test binary as the proxy daemon.
package agenttest

import (
	"testing"

	"github.com/edotau/claude-code/internal/agent"
)

// StubRouter points configure at base until t ends.
func StubRouter(t testing.TB, base string) {
	t.Cleanup(agent.StubRouter(base))
}
