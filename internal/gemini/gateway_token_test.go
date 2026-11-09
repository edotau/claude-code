package gemini

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
)

// A cached router token older than gatewayTokenMaxAge is re-minted; a fresh one is reused.
func TestGatewayTokenRemintsWhenStale(t *testing.T) {
	origMint := agent.MintSession
	mints := 0
	agent.MintSession = func(context.Context, string, int) (string, error) {
		mints++
		return "tok" + strconv.Itoa(mints), nil
	}
	t.Cleanup(func() { agent.MintSession = origMint })

	const base, profile = "http://router.test-stale", "p"
	ctx := context.Background()
	first, err := gatewayToken(ctx, base, profile)
	if err != nil || first != "tok1" {
		t.Fatalf("first mint = %q, %v", first, err)
	}
	if again, _ := gatewayToken(ctx, base, profile); again != "tok1" || mints != 1 {
		t.Fatalf("fresh token not reused: %q after %d mints", again, mints)
	}
	gatewayMu.Lock()
	e := gatewayTokens[base+"|"+profile]
	gatewayMu.Unlock()
	e.mu.Lock()
	e.minted = time.Now().Add(-2 * gatewayTokenMaxAge)
	e.mu.Unlock()
	got, err := gatewayToken(ctx, base, profile)
	if err != nil || got != "tok2" {
		t.Fatalf("stale token not re-minted: %q, %v", got, err)
	}
}
