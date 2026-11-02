// agent_setup.go — the Set up row every agent page opens with: sign in, verify, pin the route; stops at the first failure.
package configure

import (
	"context"
	"errors"
	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/preflight"
	"strings"
	"time"
)

// setupVerifyTimeout caps the one small real request the verify step sends.
const setupVerifyTimeout = 2 * time.Minute

// QuotaFailure reports a verify error that means exhausted credits or a rate limit, not bad credentials.
func QuotaFailure(err error) bool {
	s := err.Error()
	return strings.Contains(s, "quota") || strings.Contains(s, "rate_limit")
}

// VerifyAgent asks the agent for "pong" through `claude-code ask`, the same request preflight sends.
func VerifyAgent(ctx context.Context, agent string) error {
	bin, err := agentpkg.SelfBinary()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, setupVerifyTimeout)
	defer cancel()
	pin := preflight.Pin{Agent: agent, Status: preflight.StatusOK, Served: "n/a"}
	for _, p := range preflight.LivePongs(ctx, bin, []preflight.Pin{pin}) {
		if !p.OK {
			return errors.New(p.Err)
		}
	}
	return nil
}
