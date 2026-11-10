package helptext

import (
	"flag"
	"testing"
	"time"
)

// timeoutExplicit distinguishes a --timeout the caller typed from the flag's own zero-cost default,
// so the --tools-aware 45m default never clobbers an explicit value.
func TestTimeoutExplicit(t *testing.T) {
	newFS := func() (*flag.FlagSet, *time.Duration) {
		fs := flag.NewFlagSet("ask", flag.ContinueOnError)
		return fs, fs.Duration("timeout", 10*time.Minute, "")
	}
	fs, _ := newFS()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if Explicit(fs, "timeout") {
		t.Error("timeoutExplicit() = true with no flags parsed, want false")
	}

	fs, timeout := newFS()
	if err := fs.Parse([]string{"-timeout", "5m"}); err != nil {
		t.Fatal(err)
	}
	if !Explicit(fs, "timeout") {
		t.Error("timeoutExplicit() = false with -timeout on the command line, want true")
	}
	if *timeout != 5*time.Minute {
		t.Errorf("timeout = %v, want 5m", *timeout)
	}
}
