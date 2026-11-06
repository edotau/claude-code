package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/agent"
)

// TestVendorUsageFits80: every rendered line stays within an 80-column terminal, and gemini's block
// names its extra bridge verb (contract.md's help-text target).
func TestVendorUsageFits80(t *testing.T) {
	for _, vendor := range cliAgentNames() {
		usage := renderHelpPage(agentPage(vendor))
		for line := range strings.SplitSeq(strings.TrimRight(usage, "\n"), "\n") {
			if n := utf8.RuneCountInString(line); n > 80 {
				t.Errorf("vendorUsage(%q) line %q is %d runes, want <= 80", vendor, line, n)
			}
		}
		if vendor == "gemini" && !strings.Contains(usage, "bridge") {
			t.Errorf("vendorUsage(gemini) missing the bridge row:\n%s", usage)
		}
	}
}

// TestVendorModelsAliasRemoved: models is no longer a subverb; it falls through to the agent launch like any bare word.
func TestVendorModelsAliasRemoved(t *testing.T) {
	if agentSubverbs["models"] {
		t.Error("models must not route to cmdVendorSub")
	}
}

// TestVendorSubverbsCoverContractVerbs pins vendorSubverbs against contract.md's core verb set — a
// renamed or dropped subverb here silently stops routing. Checks presence, not exact membership, so
// a later addition (e.g. explore) doesn't need a second hand-copied list kept in lockstep with
// TestSpecTreeSubwordsMatchDispatch's own derivation off the same map.
func TestVendorSubverbsCoverContractVerbs(t *testing.T) {
	for _, w := range []string{"resume", "status", "result", "cancel", "tail", "setup", "catalog", "review", "transfer"} {
		if !agentSubverbs[w] {
			t.Errorf("vendorSubverbs missing %q", w)
		}
	}
}

func TestVendorUsageBridgeRowOnlyForGemini(t *testing.T) {
	if u := renderHelpPage(agentPage("codex")); strings.Contains(u, "bridge") {
		t.Errorf("vendorUsage(codex) must not carry the bridge row:\n%s", u)
	}
	if u := renderHelpPage(agentPage("gemini")); !strings.Contains(u, "  bridge") {
		t.Errorf("vendorUsage(gemini) missing the bridge row:\n%s", u)
	}
}

func saveResultRecord(t *testing.T, status, errMsg string) agent.Record {
	t.Helper()
	cwd, _ := os.Getwd()
	rec := agent.NewRecord("codex", agent.RunOptions{Prompt: "p"}, time.Now())
	rec.Status, rec.Error = status, errMsg
	rec.Repo = agent.RepoRoot(context.Background(), cwd)
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestVendorResultErrorRecordExits1(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := saveResultRecord(t, "error", "boom happened")
	for _, args := range [][]string{{rec.ID}, {"--format", "json", rec.ID}} {
		code, _, stderr := captureOutput(t, func() int { return agentResult("codex", args) })
		if code != 1 || !strings.Contains(stderr, "error: boom happened") {
			t.Errorf("args %v: exit %d stderr %q, want 1 and \"error: boom happened\"", args, code, stderr)
		}
	}
}

func TestVendorResultOKRecordExits0(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := saveResultRecord(t, "ok", "")
	code, _, stderr := captureOutput(t, func() int { return agentResult("codex", []string{rec.ID}) })
	if code != 0 || strings.Contains(stderr, "error:") {
		t.Errorf("exit %d stderr %q, want 0 and no error line", code, stderr)
	}
}

// TestVendorUsageNamesTail: every vendor's help block carries a tail row.
func TestVendorUsageNamesTail(t *testing.T) {
	for _, vendor := range cliAgentNames() {
		if !strings.Contains(renderHelpPage(agentPage(vendor)), "tail") {
			t.Errorf("vendorUsage(%q) has no tail row", vendor)
		}
	}
}

// The per-vendor help names the harness profile pin and shows the NAME placeholder on the launch --profile flag.
func TestVendorHelpHarnessProfileRows(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	for _, vendor := range []string{"codex", "gemini"} {
		text := renderHelpPage(agentPage(vendor))
		if !strings.Contains(text, "show or set "+vendor+"'s harness profile pin") {
			t.Errorf("%s help lacks the harness profile pin row:\n%s", vendor, text)
		}
		want := "harness profile for this launch"
		if vendor == "codex" {
			want += " (codex's own: -p NAME)"
		}
		var row string
		for line := range strings.SplitSeq(text, "\n") {
			if strings.Contains(line, "--profile NAME") {
				row = line
			}
		}
		if row == "" {
			t.Fatalf("%s help has no `--profile NAME` flag row:\n%s", vendor, text)
		}
		if !strings.Contains(strings.Join(strings.Fields(text), " "), want) {
			t.Errorf("%s help lacks flag usage %q:\n%s", vendor, want, text)
		}
		checkCommandRows(t, text)
		for line := range strings.SplitSeq(text, "\n") {
			if w := termWidth(line); w > 80 {
				t.Errorf("%s help line %q has %d columns, want <=80", vendor, line, w)
			}
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
	_, _, stderr := captureOutput(t, func() int { return agentVerb(notAVendor, []string{"--", "status"}) })
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
		_, _, stderr := captureOutput(t, func() int { return agentVerb(notAVendor, []string{sub}) })
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
	_, stdout, _ := captureOutput(t, func() int { return agentVerb(notAVendor, []string{"setup"}) })
	if !strings.Contains(stdout, notAVendor+"  agent  fail") {
		t.Errorf("vendorVerb(%q, [setup]) stdout = %q, want cmdVendorSetup's report row", notAVendor, stdout)
	}
}

// catalog reaches cmdVendorModels, not launchVendor, with canonical diagnostics and help.
func TestVendorVerbRoutesCatalogToCmdVendorModels(t *testing.T) {
	for _, sub := range []string{"catalog"} {
		code, _, stderr := captureOutput(t, func() int { return agentVerb("codex", []string{sub, "unexpected"}) })
		if code != 2 || !strings.Contains(stderr, "codex catalog: unexpected argument") {
			t.Errorf("vendorVerb(%s): code=%d stderr=%q", sub, code, stderr)
		}
		code, _, stderr = captureOutput(t, func() int { return agentVerb("codex", []string{sub, "--help"}) })
		if code != 0 || !strings.HasPrefix(stderr, "usage: claude-code codex catalog") {
			t.Errorf("vendorVerb(%s --help): code=%d stderr=%q", sub, code, stderr)
		}
	}
}

// TestVendorVerbRoutesResumeToCmdVendorResume: `resume` with no id hits cmdVendorResume's own usage
// check before ever reaching cmdAsk (which would otherwise dispatch a real agent run).
func TestVendorVerbRoutesResumeToCmdVendorResume(t *testing.T) {
	_, _, stderr := captureOutput(t, func() int { return agentVerb("codex", []string{"resume"}) })
	if !strings.Contains(stderr, "codex resume: usage:") {
		t.Errorf("vendorVerb(resume, no id) stderr = %q, want cmdVendorResume's usage message", stderr)
	}
}

// TestVendorVerbBareLaunchesEvenWhenFirstWordLooksLikeASubverb: an unknown bare word (not "--", not
// "ask", not in vendorSubverbs) falls through to launchVendor with args forwarded unchanged.
func TestVendorVerbBareLaunchesEvenWhenFirstWordLooksLikeASubverb(t *testing.T) {
	_, _, stderr := captureOutput(t, func() int { return agentVerb(notAVendor, []string{"--native-flag", "x"}) })
	if !strings.Contains(stderr, "unknown agent") {
		t.Errorf("bare launch stderr = %q, want agent.Vendor's unknown-vendor message", stderr)
	}
}

// TestVendorVerbHelpFlag checks that vendor help uses the shared page and exits successfully.
func TestVendorVerbHelpFlag(t *testing.T) {
	for _, flagArg := range []string{"-h", "--help", "help"} {
		code, _, stderr := captureOutput(t, func() int { return agentVerb("codex", []string{flagArg}) })
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

// seedTailJob saves a finished codex record whose log holds body and returns its id.
func seedTailJob(t *testing.T, status, body string) string {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cwd := t.TempDir()
	logPath := filepath.Join(cwd, "job.log")
	if err := os.WriteFile(logPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := agent.NewRecord("codex", agent.RunOptions{Prompt: "tail me"}, time.Now())
	rec.Repo, rec.Status, rec.Log = cwd, status, logPath
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	return rec.ID
}

// TestVendorTailPrintsFinishedLog: tail <id> on a finished ok job prints its log and exits 0.
func TestVendorTailPrintsFinishedLog(t *testing.T) {
	id := seedTailJob(t, "ok", "line one\nline two\n")
	code, stdout, stderr := captureOutput(t, func() int { return agentVerb("codex", []string{"tail", id}) })
	if code != 0 || stdout != "line one\nline two\n" {
		t.Errorf("tail ok job = exit %d stdout %q stderr %q, want exit 0 with the log verbatim", code, stdout, stderr)
	}
}

// TestVendorTailErrorJobExitsOne: a job that ended in error still prints its log but exits 1.
func TestVendorTailErrorJobExitsOne(t *testing.T) {
	id := seedTailJob(t, "error", "boom\n")
	code, stdout, _ := captureOutput(t, func() int { return agentVerb("codex", []string{"tail", id}) })
	if code != 1 || !strings.Contains(stdout, "boom") {
		t.Errorf("tail error job = exit %d stdout %q, want exit 1 with the log", code, stdout)
	}
}

// TestVendorTailUnknownIDFails: an id matching no job errors with the "<v> tail:" prefix, exit 1.
func TestVendorTailUnknownIDFails(t *testing.T) {
	seedTailJob(t, "ok", "x\n")
	code, _, stderr := captureOutput(t, func() int { return agentVerb("codex", []string{"tail", "zzzzzzzz"}) })
	if code != 1 || !strings.Contains(stderr, "codex tail:") {
		t.Errorf("tail unknown id = exit %d stderr %q, want exit 1 and the \"codex tail:\" prefix", code, stderr)
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
