package copilot

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/usage"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// An unreadable registry hides any GitHub pin, so the launch must be refused, never run on another account's token.
func TestCopilotLaunchTokenRefusesOnRegistryLoadError(t *testing.T) {
	hosttest.PinScratch(t)
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	t.Setenv("GH_TOKEN", "other-account-token")
	vaulttest.Put(t, vault.Overlay, "providers", "{not json")

	tok, err := copilotLaunchTokenFor("")
	if err == nil || tok != "" {
		t.Fatalf("copilotLaunchTokenFor(\"\") = (%q, %v), want an error and no token", tok, err)
	}
	if _, got, _, err := catalogAuth(""); err == nil || got != "" {
		t.Errorf("catalogAuth(\"\") = (%q, %v), want an error and no token on a registry load error", got, err)
	}
}

func TestCopilotTier(t *testing.T) {
	orig := copilotTierCmd
	t.Cleanup(func() { copilotTierCmd = orig })
	cases := []struct {
		name, out string
		err       error
		want      string
	}{
		{"individual", `{"copilot_plan":"individual"}`, nil, "Pro"},
		{"individual_pro", `{"copilot_plan":"individual_pro"}`, nil, "Pro+"},
		{"free", `{"copilot_plan":"free"}`, nil, "Free"},
		{"business", `{"copilot_plan":"business"}`, nil, "Business"},
		{"enterprise", `{"copilot_plan":"enterprise"}`, nil, "Enterprise"},
		{"unknown passes through", `{"copilot_plan":"new_tier"}`, nil, "new_tier"},
		{"empty plan", `{"copilot_plan":""}`, nil, ""},
		{"bad json", `nope`, nil, ""},
		{"command error", ``, errors.New("gh: not found"), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			copilotPlanMemo.Drop()
			copilotTierCmd = func(context.Context, string) ([]byte, error) { return []byte(c.out), c.err }
			if got := providers.CopilotPlanName(copilotRawPlan("", "")); got != c.want {
				t.Errorf("copilotTier = %q, want %q", got, c.want)
			}
		})
	}
}

// A seat checked inside StaleAfter reads unverified without a refresh; an unchecked one refreshes under the 1 s cap.
func TestCopilotUsedPctBacksOffAndCapsRefresh(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := router.MarkChecked("github·checked", "unlimited", time.Now()); err != nil {
		t.Fatal(err)
	}
	orig := RefreshAccount
	t.Cleanup(func() { RefreshAccount = orig })
	var calls []string
	RefreshAccount = func(ctx context.Context, key string) bool {
		calls = append(calls, key)
		if d, ok := ctx.Deadline(); !ok || time.Until(d) > refreshCap {
			t.Errorf("refresh of %s ran without the %v cap", key, refreshCap)
		}
		return false
	}
	for _, login := range []string{"checked", "fresh"} {
		if pct, ok := copilotUsedPct(context.Background(), login); ok {
			t.Errorf("copilotUsedPct(%s) = %v, verified; want unverified", login, pct)
		}
	}
	if len(calls) != 1 || calls[0] != "github·fresh" {
		t.Errorf("refreshed %v, want only github·fresh", calls)
	}
}

// An unreadable registry may hide a GitHub pin: the presence gate and headroom must say so, not read it as unpinned.
func TestUnreadableRegistryIsNotReadAsUnpinned(t *testing.T) {
	hosttest.PinScratch(t)
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	t.Setenv("GH_TOKEN", "other-account-token")
	vaulttest.Put(t, vault.Overlay, "providers", "{not json")
	if st := (copilotEngine{}).AuthStatus(); st.Err == nil {
		t.Errorf("AuthStatus = %+v, want the registry load error reported", st)
	}
	if detail, ok := pinnedHeadroom(context.Background(), ""); ok || !strings.Contains(detail, "cannot read the provider registry") {
		t.Errorf("pinnedHeadroom = %q, %v; want unworkable naming the unreadable registry", detail, ok)
	}
}

func TestCopilotTierCmdScrubsInheritedSecrets(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("PATH", dir)
	t.Setenv("HARNESS_KEEP_ENV", "")
	t.Setenv("ANTHROPIC_API_KEY", "fake-inherited-canary")
	t.Setenv("GH_TOKEN", "fake-inherited-github-token")
	script := "#!/bin/sh\nprintf '%s\\n' \"$GH_TOKEN\" \"${ANTHROPIC_API_KEY+set}\"\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := copilotTierCmd(ctx, "fake-explicit-github-token")
	if err != nil {
		t.Fatalf("real gh child: %v", err)
	}
	if string(got) != "fake-explicit-github-token\n\n" {
		t.Fatalf("gh child environment = %q; want explicit GH_TOKEN and ANTHROPIC_API_KEY unset", got)
	}
}

// The plan lookup keeps copilot's own GitHub login but no registry credential and no other agent's login.
func TestCopilotTierCmdKeepsOnlyCopilotLogin(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"preset":"databricks",`+
		`"host":"https://dev.cloud.databricks.com","auth":{"type":"key","env":"DEV_WS_TOKEN"}}}}}`)
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	for _, k := range []string{"DEV_WS_TOKEN", "OPENCODE_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		t.Setenv(k, "SENTINEL")
	}
	dump := filepath.Join(dir, "env")
	script := "#!/bin/sh\n/usr/bin/env > " + dump + "\necho '{\"copilot_plan\":\"individual_pro\"}'\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if got, err := copilotTierCmd(context.Background(), ""); err != nil || string(got) != "{\"copilot_plan\":\"individual_pro\"}\n" {
		t.Fatalf("gh api = %q, %v; want the plan JSON", got, err)
	}
	env, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"DEV_WS_TOKEN", "OPENCODE_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if strings.Contains("\n"+string(env), "\n"+k+"=") {
			t.Errorf("gh api inherited %s", k)
		}
	}
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if !strings.Contains("\n"+string(env), "\n"+k+"=SENTINEL") {
			t.Errorf("gh api lost copilot's own login %s", k)
		}
	}
}

// A /user body past the 64 KiB cap reads as a failed lookup, never a login decoded from an unbounded read.
func TestFetchGitHubLoginCapsBody(t *testing.T) {
	big := `{"login":"` + strings.Repeat("a", 128<<10) + `"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("big") != "" {
			_, _ = io.WriteString(w, big)
			return
		}
		_, _ = io.WriteString(w, `{"login":"octocat"}`)
	}))
	t.Cleanup(srv.Close)
	orig := gitHubUserURL
	t.Cleanup(func() { gitHubUserURL = orig })
	gitHubUserURL = srv.URL
	if got := fetchGitHubLogin("fake-token"); got != "octocat" {
		t.Fatalf("small body login = %q, want octocat", got)
	}
	gitHubUserURL = srv.URL + "?big=1"
	if got := fetchGitHubLogin("fake-token"); got != "" {
		t.Fatalf("oversized body gave a %d-byte login, want \"\" (body uncapped)", len(got))
	}
}

// An unworkable seat's notice carries the last check's note: a Free seat says it has no premium credits plainly.
func TestCopilotHeadroomNamesCheckNote(t *testing.T) {
	hosttest.PinScratch(t)
	gh := t.TempDir()
	t.Setenv("GH_CONFIG_DIR", gh)
	if err := os.WriteFile(filepath.Join(gh, "hosts.yml"), []byte("github.com:\n    users:\n        free:\n            oauth_token: x\n        gone:\n            oauth_token: x\n    user: free\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := CopilotUsedPct
	CopilotUsedPct = func(context.Context, string) (float64, bool) { return 0, false }
	t.Cleanup(func() { CopilotUsedPct = orig })
	for login, c := range map[string]struct{ note, want string }{
		"free": {usage.NoPremiumCredits + " (Free plan)", "free has no Copilot premium credits (Free plan)"},
		"gone": {"unavailable: no token", "gone unverified (unavailable: no token)"},
	} {
		if err := router.MarkChecked("github·"+login, c.note, time.Now()); err != nil {
			t.Fatal(err)
		}
		if got, ok := copilotHeadroom(context.Background(), login); ok || got != c.want {
			t.Errorf("copilotHeadroom(%s) = %q, %v; want %q, unworkable", login, got, ok, c.want)
		}
	}
}
