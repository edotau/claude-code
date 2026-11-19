package waves

import (
	"context"
	"strings"
	"testing"
)

func TestVerifyTaskLines(t *testing.T) {
	dir := t.TempDir()
	in := TaskVerifyInput{Dir: dir, OutDir: dir}
	pass := runCheck(context.Background(), in, "1.1", "verify", "true").Line()
	fail := runCheck(context.Background(), in, "1.2", "verify", "false").Line()
	if !strings.HasPrefix(pass, "verify 1.1: ") || !strings.Contains(pass, " ok (") {
		t.Errorf("pass line = %q", pass)
	}
	if !strings.HasPrefix(fail, "verify 1.2: ") || !strings.Contains(fail, "FAIL exit 1") {
		t.Errorf("fail line = %q", fail)
	}
}
