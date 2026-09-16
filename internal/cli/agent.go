package cli

import "errors"

// OWNER: agent-runner workstream. Replace these stubs.

var errNotImplemented = errors.New("not implemented")

func cmdAsk(args []string) int    { return fail("ask: not implemented") }
func cmdAgents(args []string) int { return fail("agents: not implemented") }
