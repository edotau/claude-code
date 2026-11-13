package ask

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

// armRateFixture is armChildEnvFixture with a codex fake that answers 429 for each workspace in limited, 402 for
// each in broke, and "hello" elsewhere; it returns the dump dir of the launches (one file per launch).
func armRateFixture(t *testing.T, limited, broke []string) string {
	t.Helper()
	dump := armChildEnvFixture(t)
	var cond strings.Builder
	for _, w := range limited {
		fmt.Fprintf(&cond, `*"/p/%s/"*) echo '429 Too Many Requests: rate limit exceeded' >&2; exit 1;; `, w)
	}
	for _, w := range broke {
		fmt.Fprintf(&cond, `*"/p/%s/"*) echo '402 Payment Required: monthly quota exceeded' >&2; exit 1;; `, w)
	}
	pathstest.FakeVendor(t, "codex", `case "$1 $2" in login*) echo "Logged in using ChatGPT"; exit 0;; `+
		`--version*|*--help*|debug*) echo 1.0.0; exit 0;; esac`+"\n"+`n=$(ls `+dump+` | wc -l | tr -d ' ')`+"\n"+
		`all="$* $(env)"; echo "$all" > `+dump+`/$n-codex`+"\n"+
		`cat > /dev/null; case "$all" in `+cond.String()+`*) echo hello;; esac`)
	origRetry := [3]time.Duration{retry.DefaultBaseDelay, retry.DefaultMaxDelay, retry.RateLimitFloor}
	retry.DefaultBaseDelay, retry.DefaultMaxDelay, retry.RateLimitFloor = time.Millisecond, 2*time.Millisecond, time.Millisecond
	retry.ResetCooldowns()
	origFindings := armFindings
	armFindings = noCredential(nil)
	t.Cleanup(func() {
		retry.DefaultBaseDelay, retry.DefaultMaxDelay, retry.RateLimitFloor = origRetry[0], origRetry[1], origRetry[2]
		armFindings = origFindings
		retry.ResetCooldowns()
	})
	t.Setenv(providers.EnvSessionProfile, "")
	t.Setenv(agentconfig.OverlayVar, "")
	return dump
}

// launchedOn is the workspace of each codex launch in the dump dir, in launch order.
func launchedOn(t *testing.T, dump string) []string {
	t.Helper()
	_, envs := armDumps(t, dump)
	var out []string
	for _, e := range envs {
		all := strings.Join(e, "\n")
		switch {
		case strings.Contains(all, "/p/dev/"):
			out = append(out, "dev")
		case strings.Contains(all, "/p/qa/"):
			out = append(out, "qa")
		default:
			out = append(out, "?")
		}
	}
	return out
}

func askAuto(t *testing.T, profile, fallback string) (int, map[string]any, string) {
	t.Helper()
	ov := `{"provider":"databricks","profile":"` + profile + `","fallback":"` + fallback + `"}`
	code, out, stderr := runCLI(t, "ask", "--agent", "codex", "--overlay", ov, "--format", "json", "go")
	var env map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &env); err != nil {
		t.Fatalf("exit %d: stdout is not one envelope: %v\n%s\n%s", code, err, out, stderr)
	}
	return code, env, stderr
}

func hopProfiles(env map[string]any) []string {
	var out []string
	hops, _ := env["hops"].([]any)
	for _, h := range hops {
		out = append(out, h.(map[string]any)["profile"].(string))
	}
	return out
}

// A 429 on the auto-picked workspace cools it and re-runs the same agent on the next one; both are listed, in order.
func TestAutoRateLimitMovesToNextWorkspace(t *testing.T) {
	dump := armRateFixture(t, []string{"dev"}, nil)
	code, env, stderr := askAuto(t, "auto", "none")
	if code != 0 || env["status"] != "ok" || env["agent"] != "codex" {
		t.Fatalf("exit %d status %v agent %v, want an ok codex answer\n%s", code, env["status"], env["agent"], stderr)
	}
	if got := launchedOn(t, dump); got[0] != "dev" || got[len(got)-1] != "qa" {
		t.Errorf("launches on %v, want dev first and qa last", got)
	}
	checkMovedReport(t, env)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	if until := router.LoadState().CoolingUntil(router.AccountOf(reg.Providers["dev"])); !until.After(time.Now()) {
		t.Errorf("dev cooling until %v, want a hold after the 429", until)
	}
}

// checkMovedReport asserts the envelope lists the hops dev then qa and names the move in ran_on.
func checkMovedReport(t *testing.T, env map[string]any) {
	t.Helper()
	if got := strings.Join(hopProfiles(env), ","); !strings.HasPrefix(got, "dev") || !strings.HasSuffix(got, "qa") {
		t.Errorf("hops %q, want every workspace tried in order: dev then qa", got)
	}
	if ran, _ := env["ran_on"].(string); !strings.Contains(ran, "tried dev → qa") {
		t.Errorf("ran_on %q, want it to name dev → qa", ran)
	}
}

// Every workspace limited: --fallback none still fails, naming each workspace tried and keeping the rate_limit kind.
func TestAutoRateLimitEveryWorkspaceFails(t *testing.T) {
	dump := armRateFixture(t, []string{"dev", "qa"}, nil)
	code, env, stderr := askAuto(t, "auto", "none")
	msg, _ := env["error"].(map[string]any)["message"].(string)
	kind, _ := env["error"].(map[string]any)["kind"].(string)
	if code == 0 || kind != "rate_limit" || !strings.Contains(msg, "tried dev, qa") {
		t.Fatalf("exit %d kind %q message %q, want a rate_limit failure naming dev, qa\n%s", code, kind, msg, stderr)
	}
	if got := strings.Join(launchedOn(t, dump), ","); !strings.Contains(got, "dev") || !strings.Contains(got, "qa") {
		t.Errorf("launches %q, want both workspaces asked", got)
	}
}

// An explicitly named workspace never moves: its 429 is the failure.
func TestExplicitProfileRateLimitStays(t *testing.T) {
	dump := armRateFixture(t, []string{"dev"}, nil)
	code, env, _ := askAuto(t, "dev", "none")
	if code == 0 || env["error"] == nil {
		t.Fatalf("exit %d, want the rate limit to fail the ask", code)
	}
	for _, w := range launchedOn(t, dump) {
		if w != "dev" {
			t.Errorf("launches %v, want only dev", launchedOn(t, dump))
		}
	}
}

// A failure that is not a rate limit stays on its workspace: only the fallback policy applies.
func TestAutoNonRateLimitFailureDoesNotMove(t *testing.T) {
	dump := armRateFixture(t, nil, []string{"dev"})
	if code, _, _ := askAuto(t, "auto", "none"); code == 0 {
		t.Fatal("a 402 on the picked workspace answered, want the failure")
	}
	for _, w := range launchedOn(t, dump) {
		if w != "dev" {
			t.Errorf("launches %v, want only dev: a 402 is not a rate limit", launchedOn(t, dump))
		}
	}
}

// With the default fallback the 429 still moves workspaces first: codex answers on qa, no other agent runs.
func TestAutoRateLimitMovesBeforeAgentFallback(t *testing.T) {
	dump := armRateFixture(t, []string{"dev"}, nil)
	code, env, stderr := askAuto(t, "auto", "all")
	bins, _ := armDumps(t, dump)
	if code != 0 || env["agent"] != "codex" || env["fallback"] != nil {
		t.Fatalf("exit %d agent %v fallback %v, want codex answering on qa with no fallback\n%s", code, env["agent"], env["fallback"], stderr)
	}
	for _, b := range bins {
		if b != "codex" {
			t.Errorf("launches %v, want only codex before any agent fallback", bins)
		}
	}
}

// A 402 on a probe attempt re-runs the same workspace under the fallback policy the probe skipped, never another one.
func TestAutoProbeFailureAppliesFallbackPolicyOnSameWorkspace(t *testing.T) {
	dump := armRateFixture(t, nil, []string{"dev"})
	askAuto(t, "auto", "all")
	bins, _ := armDumps(t, dump)
	codex := 0
	for _, b := range bins {
		if b == "codex" {
			codex++
		}
	}
	if codex != 2 {
		t.Errorf("launches %v, want codex on dev twice: the probe, then the fallback policy", bins)
	}
	for _, w := range launchedOn(t, dump) {
		if w == "qa" {
			t.Errorf("launches %v: a 402 moved to qa", launchedOn(t, dump))
		}
	}
}
