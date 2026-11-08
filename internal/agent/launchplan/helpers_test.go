package launchplan_test

import (
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const (
	testRouterBase   = "http://127.0.0.1:9999"
	testRouterSecret = "rtr-secret-xyz"
)

func hermetic(t *testing.T, userProviders string) {
	t.Helper()
	hosttest.HermeticEnv(t)
	if userProviders != "" {
		vaulttest.Put(t, vault.Overlay, "providers", userProviders)
	}
	t.Cleanup(agent.StubRouter("http://127.0.0.1:4000"))
}
