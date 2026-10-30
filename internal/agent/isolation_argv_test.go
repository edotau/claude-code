package agent

import (
	"slices"
	"testing"
)

func TestIsolationFlagsOnlyWhenRequested(t *testing.T) {
	has := func(argv []string, flags ...string) bool {
		for _, f := range flags {
			if !slices.Contains(argv, f) {
				return false
			}
		}
		return true
	}
	cx := codexHeadless(nil, &Headless{})
	if has(cx, "--ephemeral") || has(cx, "--ignore-user-config") || has(cx, "--ignore-rules") {
		t.Fatalf("codex flags leaked: %v", cx)
	}
	cx = codexHeadless(nil, &Headless{NoPersist: true, SafeMode: true})
	if !has(cx, "--ephemeral", "--ignore-user-config", "--ignore-rules") {
		t.Fatalf("codex argv = %v", cx)
	}
	if _, argv := opencodeRun("", Headless{Prompt: "x"}); has(argv, "--pure") {
		t.Fatalf("opencode --pure leaked: %v", argv)
	}
	if _, argv := opencodeRun("", Headless{Prompt: "x", SafeMode: true}); !has(argv, "--pure") {
		t.Fatalf("opencode argv = %v", argv)
	}
}
