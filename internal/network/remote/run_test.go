package remote

import (
	"encoding/base64"
	"slices"
	"strings"
	"testing"
)

// decodedScript recovers the shell script hostArgv base64-encoded into ssh's last argument.
func decodedScript(t *testing.T, argv []string) string {
	t.Helper()
	last := argv[len(argv)-1]
	start := strings.Index(last, "printf %s ") + len("printf %s ")
	end := strings.Index(last, " | base64 -d")
	b, err := base64.StdEncoding.DecodeString(last[start:end])
	if err != nil {
		t.Fatalf("script not base64: %v (%q)", err, last)
	}
	return string(b)
}

func TestHostArgvPlainVerb(t *testing.T) {
	argv := hostArgv("wks", []string{"vault", "ls", "it's"}, true)
	if !slices.Contains(argv, "-t") || slices.ContainsFunc(argv, func(a string) bool { return a == "-L" }) {
		t.Errorf("want -t and no tunnel: %q", argv)
	}
	if i := slices.Index(argv, "--"); i < 0 || argv[i+1] != "wks" {
		t.Errorf("host must follow --: %q", argv)
	}
	script := decodedScript(t, argv)
	if !strings.Contains(script, PathPrefix) || !strings.Contains(script, `'it'\''s'`) {
		t.Errorf("script = %q", script)
	}
}

func TestSplitHostErrors(t *testing.T) {
	for _, args := range [][]string{nil, {"--host"}, {"--host", "-oProxyCommand=x", "version"}, {"--host", "wks"}, {"--host="}} {
		if _, _, err := splitHost(args); err == nil {
			t.Errorf("splitHost(%q) accepted", args)
		}
	}
	for _, args := range [][]string{{"--host=wks", "doctor", "--prompts"}, {"--host", "wks", "doctor", "--prompts"}} {
		host, rest, err := splitHost(args)
		if err != nil || host != "wks" || !slices.Equal(rest, []string{"doctor", "--prompts"}) {
			t.Errorf("%q: %q %q %v", args, host, rest, err)
		}
	}
}

func TestOpenerOnWSLUsesWindowsBrowser(t *testing.T) {
	origOS, origWSL := goos, isWSL
	goos, isWSL = "linux", func() bool { return true }
	t.Cleanup(func() { goos, isWSL = origOS, origWSL })
	t.Setenv("PATH", t.TempDir()) // no wslview
	if got := opener(); got != wslExplorer {
		t.Errorf("opener on WSL = %q, want %q", got, wslExplorer)
	}
	isWSL = func() bool { return false }
	if got := opener(); got != "xdg-open" {
		t.Errorf("opener off WSL = %q, want xdg-open", got)
	}
}
