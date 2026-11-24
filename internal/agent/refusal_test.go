package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/router"
)

var errPolicy = &retry.Error{Kind: retry.KindFatal, PolicyRefusal: true, Err: errors.New("content_filter")}

// refusalFixture is the cross-family row xf and a Sonnet cell served on it.
func refusalFixture(t *testing.T) (Dispatch, RunOptions) {
	t.Helper()
	_, _, pair := autoFixture(t)
	disp := Dispatch{Agent: &fakeRunner{name: "opencode"}, Account: router.ProviderAccount("xf")}
	return disp, RunOptions{Model: pair[0], CellModel: true, Fallback: router.FallbackAll}
}

func policyPlanFor(disp Dispatch, req RunOptions, hops []wire.Hop) repickPlan {
	return planRepick(context.Background(), refusedHop{disp: disp, req: req, err: errPolicy, hops: hops})
}

// A Sonnet policy refusal re-picks one model of another family on the same profile and records both families.
func TestPlanRepickPolicyRefusalChangesFamily(t *testing.T) {
	disp, req := refusalFixture(t)
	plan := policyPlanFor(disp, req, nil)
	if plan.pmodel == "" || plan.prefusal != "" || !router.DifferentFamily(plan.pmodel, req.Model) {
		t.Fatalf("plan = %+v, want a model of another family", plan)
	}
	h := plan.record(wire.Hop{})
	if len(h.Errors) != 1 || !strings.HasPrefix(h.Errors[0].Message, policyNote) ||
		!strings.Contains(h.Errors[0].Message, models.FamilyName(plan.pmodel)) {
		t.Fatalf("hop errors = %+v, want the reason with both families", h.Errors)
	}
	t.Logf("(a) fallback agents: %s", plan.note())
}

// An explicit choice is kept: the refusal names the other families; a second refusal names both.
func TestPlanRepickPolicyRefusalKeepsExplicitAndStopsAfterOne(t *testing.T) {
	disp, req := refusalFixture(t)
	none := req
	none.Fallback = router.FallbackNone
	plan := policyPlanFor(disp, none, nil)
	if plan.pick() != "" || !strings.Contains(plan.prefusal, "kept, not replaced") || strings.HasSuffix(plan.prefusal, "none") {
		t.Fatalf("--fallback none plan = %+v, want the refusal naming other ids", plan)
	}
	t.Logf("(b) fallback none: %v", plan.refuse(errPolicy))
	first := policyPlanFor(disp, req, nil)
	hops := []wire.Hop{first.record(wire.Hop{})}
	if again := policyPlanFor(disp, req, hops); again.pick() != "" {
		t.Fatalf("second refusal re-picked %q, want none", again.pick())
	}
	err := refusedTwice(hops, errPolicy)
	if !strings.Contains(err.Error(), "twice") || !strings.Contains(err.Error(), first.pmodel) || !retry.IsPolicyRefusal(err) {
		t.Fatalf("err = %v, want both refusals named", err)
	}
	t.Logf("(c) second refusal: %v", err)
}

// A confined dispatch (an explicit profile) re-picks another family only on its own workspace's account.
func TestPlanRepickPolicyRefusalStaysOnWorkspace(t *testing.T) {
	disp, req := refusalFixture(t)
	req.Provider = "xf"
	var accounts []string
	orig := policyCandsFn
	policyCandsFn = func(d Dispatch, tools bool, account, ran string) []string {
		accounts = append(accounts, account)
		return orig(d, tools, account, ran)
	}
	t.Cleanup(func() { policyCandsFn = orig })
	plan := policyPlanFor(disp, req, nil)
	if plan.pmodel == "" || len(accounts) != 1 || accounts[0] != disp.Account {
		t.Fatalf("plan %+v asked accounts %v, want one ask on %s", plan, accounts, disp.Account)
	}
}
