package shell_test

import (
	"reflect"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/shell"
)

func TestScriptRedirectTokenReuse(t *testing.T) {
	prefix := "printf '%s\\n' 'git status; echo x > /tmp/token-write; rm /tmp/token-remove' > /tmp/token-script; "
	for _, suffix := range []string{
		"",
		" < /dev/null",
		" > /dev/null",
		" '&'",
		" '<' '>'",
		" 2>&1",
		" <<'EOF'\nignored\nEOF\n",
		" <<-EOF\n\tignored\n\tEOF\n",
	} {
		t.Run(suffix, func(t *testing.T) {
			want := scriptConsumerResults(prefix + "sh /tmp/token-script")
			got := scriptConsumerResults(prefix + "sh /tmp/token-script" + suffix)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("consumer results = %#v, want %#v", got, want)
			}
			if len(got[0].([][]string)) == 0 || len(got[1].([]string)) == 0 || len(got[2].([]string)) == 0 {
				t.Fatal("expected git, write and removal observations")
			}
		})
	}
}

func scriptConsumerResults(cmd string) [3]any {
	var calls [][]string
	shell.EachGit(cmd, "/tmp", func(dir, verb string, args []string) bool {
		calls = append(calls, append([]string{dir, verb}, args...))
		return false
	})
	return [3]any{calls, core.WrittenPaths(cmd, "/tmp"), core.RemovedPaths(cmd, "/tmp")}
}

func BenchmarkScriptRedirectTokenReuse(b *testing.B) {
	for b.Loop() {
		scriptConsumerResults("printf '%s\\n' 'git status; echo x > /tmp/token-write; rm /tmp/token-remove' > /tmp/token-script; sh /tmp/token-script")
	}
}
