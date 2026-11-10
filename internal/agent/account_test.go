package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// codex served by an OpenAI API key bills openai·openai (credits), not the ChatGPT plan's key.
func TestResolveAccountNamesActualBackend(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.SetRecord(t, "claude", "profile", "openai")
	t.Setenv("OPENAI_API_KEY", "sk-test-not-real")
	acct, billing := resolveAccount("codex", router.Cell{Model: "gpt-6.1-sol", Account: "codex:chatgpt"})
	if acct != "openai·openai" || billing != router.BillCredits {
		t.Fatalf("resolveAccount = (%q, %q), want openai·openai credits", acct, billing)
	}
	if a, _ := resolveAccount("opencode", router.Cell{Model: "opencode-go/kimi-k3.1-pro"}); a != "" {
		t.Errorf("opencode keeps its table account, got %q", a)
	}
}

// The session's profile (env) beats the vault pin, and a codex login cell keeps its own account.
func TestResolveAccountFollowsSessionProfile(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.SetRecord(t, "claude", "profile", "anthropic")
	t.Setenv("OPENAI_API_KEY", "sk-test-not-real")
	t.Setenv(providers.EnvSessionProfile, "openai")
	if acct, _ := resolveAccount("claude", router.Cell{Model: "claude-sonnet-5-5"}); acct != "openai·openai" {
		t.Errorf("claude account = %q, want the session profile openai·openai", acct)
	}
	if acct, _ := resolveAccount("codex", router.Cell{Model: "gpt-6.1-sol", Account: "openai·chatgpt"}); acct != "" {
		t.Errorf("codex login cell account = %q, want it kept (empty override)", acct)
	}
}

// TestProbeCLIClassify runs a real child: stdout vs combined output, and a chosen exit classifies as NeedsLogin.
func TestProbeCLIClassify(t *testing.T) {
	ctx := context.Background()
	spec := EnvSpec{Mode: EnvAgent}
	script := "echo out; echo 'bad login' >&2; exit 3"
	out, err := ProbeCLI(ctx, "/bin/sh", spec, false, "-c", script)
	if string(out) != "out\n" || err == nil {
		t.Fatalf("Output = %q, %v; want stdout only and an exit error", out, err)
	}
	if v, d := ClassifyProbe(ctx, err); v != providers.VerdictNeedsLogin || d != "" {
		t.Errorf("exit 3 = %s %q, want needs_login", v, d)
	}
	if d := ProbeErrDetail(err); d != "exit status 3: bad login" {
		t.Errorf("ProbeErrDetail = %q, want the first stderr line appended", d)
	}
	if out, _ := ProbeCLI(ctx, "/bin/sh", spec, true, "-c", script); !strings.Contains(string(out), "bad login") {
		t.Errorf("combined output %q lacks stderr", out)
	}
}

// TestClassifyProbeTransient: a deadline and a missing binary are transient, never NeedsLogin.
func TestClassifyProbeTransient(t *testing.T) {
	ctx := context.Background()
	spec := EnvSpec{Mode: EnvAgent}
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_, err := ProbeCLI(short, "/bin/sh", spec, false, "-c", "sleep 5")
	if v, d := ClassifyProbe(short, err); v != providers.VerdictTransient || d != context.DeadlineExceeded.Error() {
		t.Errorf("deadline = %s %q, want transient with the ctx error", v, d)
	}
	_, err = ProbeCLI(ctx, "/nonexistent/probe-bin", spec, false)
	if v, _ := ClassifyProbe(ctx, err); v != providers.VerdictTransient {
		t.Errorf("missing bin = %s, want transient", v)
	}
}
