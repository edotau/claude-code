package cli

import (
	"context"
	"os"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/vendoragent"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// notAVendor is an unregistered vendor name: every vendoragent lookup and launch.Vendor's own lookup
// reject it in-process, fast, so dispatch routing can be probed without touching a real vendor binary.
const notAVendor = "zzz-not-a-vendor"

// TestVendorUsageFits80: every rendered line stays within an 80-column terminal, and gemini's block
// names its extra bridge verb (contract.md's help-text target).
func TestVendorUsageFits80(t *testing.T) {
	for _, vendor := range vendorNames() {
		usage := vendorUsage(vendor)
		for _, line := range strings.Split(strings.TrimRight(usage, "\n"), "\n") {
			if n := utf8.RuneCountInString(line); n > 80 {
				t.Errorf("vendorUsage(%q) line %q is %d runes, want <= 80", vendor, line, n)
			}
		}
		if vendor == "gemini" && !strings.Contains(usage, "bridge") {
			t.Errorf("vendorUsage(gemini) missing the bridge row:\n%s", usage)
		}
	}
}

// TestVendorVerbDoubleDashForcesPassthrough: `<v> -- status` must reach launchVendor, not
// cmdVendorStatus, even though "status" is also a subverb word — distinguished here by message
// shape: launchVendor's failure is launch.Vendor's generic "unknown vendor" text, never the
// "<v> status: ..." prefix cmdVendorStatus would produce.
func TestVendorVerbDoubleDashForcesPassthrough(t *testing.T) {
	_, _, stderr := captureOutput(t, func() int { return vendorVerb(notAVendor, []string{"--", "status"}) })
	if strings.Contains(stderr, notAVendor+" status:") {
		t.Errorf("-- status reached cmdVendorStatus instead of launchVendor: %q", stderr)
	}
	if !strings.Contains(stderr, "unknown vendor") {
		t.Errorf("-- status stderr = %q, want launch.Vendor's \"unknown vendor\" message", stderr)
	}
}

// TestVendorVerbRoutesStatusResultCancelToCmdVendorSub: these three subverbs reach their
// cmdVendorSub handlers, proven by the handler-specific "<v> <verb>: ..." error prefix that only
// appears once vendoragent's own unknown-vendor error is wrapped by that handler.
func TestVendorVerbRoutesStatusResultCancelToCmdVendorSub(t *testing.T) {
	for _, sub := range []string{"status", "result", "cancel"} {
		_, _, stderr := captureOutput(t, func() int { return vendorVerb(notAVendor, []string{sub}) })
		want := notAVendor + " " + sub + ":"
		if !strings.Contains(stderr, want) {
			t.Errorf("vendorVerb(%q, [%q]) stderr = %q, want it to contain %q", notAVendor, sub, stderr, want)
		}
	}
}

// TestVendorVerbRoutesSetupToCmdVendorSetup: cmdVendorSetup prints its report to stdout (not an
// error), so unlike the other handlers its routing proof reads stdout, keyed on rep.Vendor appearing
// as the row's first column.
func TestVendorVerbRoutesSetupToCmdVendorSetup(t *testing.T) {
	_, stdout, _ := captureOutput(t, func() int { return vendorVerb(notAVendor, []string{"setup"}) })
	if !strings.Contains(stdout, notAVendor+"  vendor  fail") {
		t.Errorf("vendorVerb(%q, [setup]) stdout = %q, want cmdVendorSetup's report row", notAVendor, stdout)
	}
}

// TestVendorVerbRoutesModelsToCmdVendorModels: `models` reaches cmdVendorModels (its own "unknown
// vendor %q" wording), not launchVendor.
func TestVendorVerbRoutesModelsToCmdVendorModels(t *testing.T) {
	_, _, stderr := captureOutput(t, func() int { return vendorVerb(notAVendor, []string{"models"}) })
	if !strings.Contains(stderr, "unknown vendor \""+notAVendor+"\"") {
		t.Errorf("vendorVerb(models) stderr = %q, want cmdVendorModels's unknown-vendor message", stderr)
	}
}

// TestVendorVerbRoutesResumeToCmdVendorResume: `resume` with no id hits cmdVendorResume's own usage
// check before ever reaching cmdAsk (which would otherwise dispatch a real agent run).
func TestVendorVerbRoutesResumeToCmdVendorResume(t *testing.T) {
	_, _, stderr := captureOutput(t, func() int { return vendorVerb("codex", []string{"resume"}) })
	if !strings.Contains(stderr, "codex resume: usage:") {
		t.Errorf("vendorVerb(resume, no id) stderr = %q, want cmdVendorResume's usage message", stderr)
	}
}

// TestVendorVerbBareLaunchesEvenWhenFirstWordLooksLikeASubverb: an unknown bare word (not "--", not
// "ask", not in vendorSubverbs) falls through to launchVendor with args forwarded unchanged.
func TestVendorVerbBareLaunchesEvenWhenFirstWordLooksLikeASubverb(t *testing.T) {
	_, _, stderr := captureOutput(t, func() int { return vendorVerb(notAVendor, []string{"--native-flag", "x"}) })
	if !strings.Contains(stderr, "unknown vendor") {
		t.Errorf("bare launch stderr = %q, want launch.Vendor's unknown-vendor message", stderr)
	}
}

// TestVendorVerbHelpFlag: -h/--help prints vendorUsage and exits 2 (matches other verbs' help convention).
func TestVendorVerbHelpFlag(t *testing.T) {
	for _, flagArg := range []string{"-h", "--help", "help"} {
		code, _, stderr := captureOutput(t, func() int { return vendorVerb("codex", []string{flagArg}) })
		if code != 2 {
			t.Errorf("vendorVerb(codex, %q) exit %d, want 2", flagArg, code)
		}
		if !strings.Contains(stderr, "claude-code codex — the codex agent") {
			t.Errorf("vendorVerb(codex, %q) stderr missing usage header: %q", flagArg, stderr)
		}
	}
}

// TestVendorSubverbsCoverContractVerbs pins vendorSubverbs against contract.md's core verb set — a
// renamed or dropped subverb here silently stops routing. Checks presence, not exact membership, so
// a later addition (e.g. explore) doesn't need a second hand-copied list kept in lockstep with
// TestSpecTreeSubwordsMatchDispatch's own derivation off the same map.
func TestVendorSubverbsCoverContractVerbs(t *testing.T) {
	for _, w := range []string{"resume", "status", "result", "cancel", "setup", "models", "review", "transfer"} {
		if !vendorSubverbs[w] {
			t.Errorf("vendorSubverbs missing %q", w)
		}
	}
}

// TestVendorCompletionOffersEverySubverb: tab-completing a vendor's own bare position (`codex ⇥`)
// offers "ask" plus every word in vendorSubverbs, for every vendor — derived off vendorSubverbs so a
// future addition (e.g. explore) is picked up without a second hand-copied list to keep in lockstep.
func TestVendorCompletionOffersEverySubverb(t *testing.T) {
	want := []string{"ask"}
	for sub := range vendorSubverbs {
		want = append(want, sub)
	}
	slices.Sort(want)
	for _, vendor := range vendorNames() {
		got := completionCandidates([]string{vendor, ""})
		slices.Sort(got)
		if vendor == "gemini" {
			got = slices.DeleteFunc(slices.Clone(got), func(s string) bool { return s == "bridge" })
		}
		if !slices.Equal(got, want) {
			t.Errorf("completionCandidates([%q \"\"]) = %v, want %v", vendor, got, want)
		}
	}
}

// TestVendorCompletionSubverbFlags: a vendor subverb's own flags complete past it, pinning two
// representative cases (status's --all and setup's --format) against their flagInfo wiring.
func TestVendorCompletionSubverbFlags(t *testing.T) {
	if got := completionCandidates([]string{"codex", "status", "--"}); !slices.Contains(got, "--all") {
		t.Errorf("codex status -- = %v, missing --all", got)
	}
	if got := completionCandidates([]string{"codex", "setup", "--"}); !slices.Contains(got, "--format") {
		t.Errorf("codex setup -- = %v, missing --format", got)
	}
}

// TestVendorCompletionDoesNotFollowDoubleDash: `<v> -- ⇥` is past the vendor's own (unknown) native
// flags — it must offer nothing, not the subverb domain again (mirrors the old `run codex --` guard).
func TestVendorCompletionDoesNotFollowDoubleDash(t *testing.T) {
	if got := completionCandidates([]string{"codex", "--", ""}); len(got) != 0 {
		t.Errorf("codex -- ⇥ = %v, want none (codex's own flags are not ours to know)", got)
	}
}

func TestVendorUsageBridgeRowOnlyForGemini(t *testing.T) {
	if u := vendorUsage("codex"); strings.Contains(u, "bridge") {
		t.Errorf("vendorUsage(codex) must not carry the bridge row:\n%s", u)
	}
	if u := vendorUsage("gemini"); !strings.Contains(u, "gemini bridge") {
		t.Errorf("vendorUsage(gemini) missing the bridge row:\n%s", u)
	}
}

func TestPrintDispatchLogPreviewCutsOnRuneBoundary(t *testing.T) {
	// Given a prompt of 4-byte runes longer than the preview; When the log prints; Then the cut line is valid UTF-8
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := agent.NewRecord("codex", agent.Request{Prompt: strings.Repeat("😀", 100)}, time.Now())
	rec.Status = "ok"
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	_, out, _ := captureOutput(t, func() int { return printDispatchLog("text") })
	if !utf8.ValidString(out) {
		t.Fatalf("output is not valid UTF-8: %q", out)
	}
	if !strings.Contains(out, "😀…") {
		t.Errorf("output lacks a truncated preview ending in …:\n%s", out)
	}
}

func saveResultRecord(t *testing.T, status, errMsg string) agent.Record {
	t.Helper()
	cwd, _ := os.Getwd()
	rec := agent.NewRecord("codex", agent.Request{Prompt: "p"}, time.Now())
	rec.Status, rec.Error = status, errMsg
	rec.Repo = vendoragent.RepoRoot(context.Background(), cwd)
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestVendorResultErrorRecordExits1(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := saveResultRecord(t, "error", "boom happened")
	for _, args := range [][]string{{rec.ID}, {"--format", "json", rec.ID}} {
		code, _, stderr := captureOutput(t, func() int { return cmdVendorResult("codex", args) })
		if code != 1 || !strings.Contains(stderr, "error: boom happened") {
			t.Errorf("args %v: exit %d stderr %q, want 1 and \"error: boom happened\"", args, code, stderr)
		}
	}
}

func TestVendorResultOKRecordExits0(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := saveResultRecord(t, "ok", "")
	code, _, stderr := captureOutput(t, func() int { return cmdVendorResult("codex", []string{rec.ID}) })
	if code != 0 || strings.Contains(stderr, "error:") {
		t.Errorf("exit %d stderr %q, want 0 and no error line", code, stderr)
	}
}
