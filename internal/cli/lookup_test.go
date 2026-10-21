package cli

import (
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// funcName is the fully-qualified name of a func value, for comparing `lookup(x).run` against the
// package-level handler it must dispatch to (func values are not comparable with ==).
func funcName(f func([]string) int) string {
	return runtime.FuncForPC(reflect.ValueOf(f).Pointer()).Name()
}

// TestLookupSecretsAndConfigureDispatchToTheRealHandlers: `commands()` must wire "secrets" and
// "configure" to cmdSecrets/cmdConfigure themselves, not a stub or a renamed/dropped handler.
func TestLookupSecretsAndConfigureDispatchToTheRealHandlers(t *testing.T) {
	cases := map[string]func([]string) int{
		"secrets":   cmdSecrets,
		"configure": cmdConfigure,
	}
	for name, want := range cases {
		c := lookup(name)
		if c == nil {
			t.Fatalf("lookup(%q) = nil", name)
		}
		if got, want := funcName(c.run), funcName(want); got != want {
			t.Errorf("lookup(%q).run = %s, want %s", name, got, want)
		}
	}
}

// TestCmdSecretsNoArgsPrintsUsageAndExitsTwo: bare `secrets` (no op) is a usage error, not a silent no-op.
func TestCmdSecretsNoArgsPrintsUsageAndExitsTwo(t *testing.T) {
	code, _, stderr := captureOutput(t, func() int { return cmdSecrets(nil) })
	if code != 2 {
		t.Fatalf("cmdSecrets(nil) = %d, want 2; stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "usage: claude-code secrets") {
		t.Errorf("stderr = %q, want usage text", stderr)
	}
}
