package agentcmd

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

// TestVendorModelsAliasRemoved: models is no longer a subverb; it falls through to the agent launch like any bare word.
func TestVendorModelsAliasRemoved(t *testing.T) {
	if Subverbs["models"] {
		t.Error("models must not route to cmdVendorSub")
	}
}

// TestVendorSubverbsCoverContractVerbs pins vendorSubverbs against contract.md's core verb set — a
// renamed or dropped subverb here silently stops routing. Checks presence, not exact membership, so
// a later addition (e.g. explore) doesn't need a second hand-copied list kept in lockstep with
// TestSpecTreeSubwordsMatchDispatch's own derivation off the same map.
func TestVendorSubverbsCoverContractVerbs(t *testing.T) {
	for _, w := range []string{"resume", "status", "result", "cancel", "tail", "setup", "catalog", "review", "transfer"} {
		if !Subverbs[w] {
			t.Errorf("vendorSubverbs missing %q", w)
		}
	}
}

// notAVendor is an unregistered vendor name: every vendoragent lookup and agent.Vendor's own lookup
// reject it in-process, fast, so dispatch routing can be probed without touching a real vendor binary.
const notAVendor = "zzz-not-a-vendor"

// TestVendorVerbDoubleDashForcesPassthrough: `<v> -- status` must reach launchVendor, not
// cmdVendorStatus, even though "status" is also a subverb word — distinguished here by message
// shape: launchVendor's failure is agent.Vendor's generic "unknown agent" text, never the
// "<v> status: ..." prefix cmdVendorStatus would produce.
func TestVendorVerbDoubleDashForcesPassthrough(t *testing.T) {
	_, _, stderr := hosttest.CaptureOutput(t, func() int { return agentVerb(notAVendor, []string{"--", "status"}) })
	if strings.Contains(stderr, notAVendor+" status:") {
		t.Errorf("-- status reached cmdVendorStatus instead of launchVendor: %q", stderr)
	}
	if !strings.Contains(stderr, "unknown agent") {
		t.Errorf("-- status stderr = %q, want agent.Vendor's \"unknown agent\" message", stderr)
	}
}

// TestVendorVerbRoutesStatusResultCancelToCmdVendorSub: these three subverbs reach their
// cmdVendorSub handlers, proven by the handler-specific "<v> <verb>: ..." error prefix that only
// appears once vendoragent's own unknown-vendor error is wrapped by that handler.
func TestVendorVerbRoutesStatusResultCancelToCmdVendorSub(t *testing.T) {
	for _, sub := range []string{"status", "result", "cancel", "tail"} {
		_, _, stderr := hosttest.CaptureOutput(t, func() int { return agentVerb(notAVendor, []string{sub}) })
		want := notAVendor + " " + sub + ":"
		if !strings.Contains(stderr, want) {
			t.Errorf("vendorVerb(%q, [%q]) stderr = %q, want it to contain %q", notAVendor, sub, stderr, want)
		}
	}
}

// TestVendorVerbBareLaunchesEvenWhenFirstWordLooksLikeASubverb: an unknown bare word (not "--", not
// "ask", not in vendorSubverbs) falls through to launchVendor with args forwarded unchanged.
func TestVendorVerbBareLaunchesEvenWhenFirstWordLooksLikeASubverb(t *testing.T) {
	_, _, stderr := hosttest.CaptureOutput(t, func() int { return agentVerb(notAVendor, []string{"--native-flag", "x"}) })
	if !strings.Contains(stderr, "unknown agent") {
		t.Errorf("bare launch stderr = %q, want agent.Vendor's unknown-vendor message", stderr)
	}
}

// TestVendorVerbHelpFlag checks that vendor help uses the shared page and exits successfully.
func TestVendorVerbHelpFlag(t *testing.T) {
	for _, flagArg := range []string{"-h", "--help", "help"} {
		code, _, stderr := hosttest.CaptureOutput(t, func() int { return agentVerb("codex", []string{flagArg}) })
		if code != 0 {
			t.Errorf("vendorVerb(codex, %q) exit %d, want 0", flagArg, code)
		}
		if !strings.HasPrefix(stderr, "usage: claude-code codex") {
			t.Errorf("vendorVerb(codex, %q) stderr missing usage header: %q", flagArg, stderr)
		}
		for _, want := range []string{"  catalog", "select the model for this launch"} {
			if !strings.Contains(stderr, want) {
				t.Errorf("vendorVerb(codex, %q) help missing %q: %s", flagArg, want, stderr)
			}
		}
	}
}

// A blurb without a conversion must render verbatim, not with a %!(EXTRA ...) tail.
func TestAgentSubPageHasNoFormatDebris(t *testing.T) {
	for verb := range agentSubPages {
		if about := agentSubPage("codex", verb).About; strings.Contains(about, "%!") || strings.Contains(about, "%s") {
			t.Errorf("%s about = %q", verb, about)
		}
	}
}

// agentVerb runs AgentVerb.
func agentVerb(name string, args []string) int { return AgentVerb(name, args) }
