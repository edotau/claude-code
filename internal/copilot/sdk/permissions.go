// The SDK box: one headless Copilot turn over the real CLI. In sdk*.go, `copilot.` is the SDK import.

package sdk

import (
	"sync/atomic"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

// permissionHandler wraps baseHandler to record an approval in approved (handlers run on SDK goroutines).
func permissionHandler(o Options, approved *atomic.Bool) copilot.PermissionHandlerFunc {
	h := baseHandler(o)
	return func(req copilot.PermissionRequest, inv copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
		d, err := h(req, inv)
		// NoResult hands the call to managed org settings, which may run it
		switch d.(type) {
		case *rpc.PermissionDecisionApproveOnce, *rpc.PermissionDecisionNoResult:
			if err == nil {
				approved.Store(true)
			}
		}
		return d, err
	}
}

func baseHandler(o Options) copilot.PermissionHandlerFunc {
	if !o.AllowAllTools {
		return denyAllTools
	}
	return approveToolOnce
}

func denyAllTools(_ copilot.PermissionRequest, _ copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
	feedback := denyFeedback
	return &rpc.PermissionDecisionReject{Feedback: &feedback}, nil
}

// approveToolOnce is the explicit opt-in; managed org settings still win.
func approveToolOnce(_ copilot.PermissionRequest, inv copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
	if inv.ManagedSettingsEnabled {
		return &rpc.PermissionDecisionNoResult{}, nil
	}
	return &rpc.PermissionDecisionApproveOnce{}, nil
}
