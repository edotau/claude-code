package websearch

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }

// TestCmdWebSearchQueryForms: positional and --query both reach OneShot (no key → 1); both at once is usage (2).
func TestCmdWebSearchQueryForms(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"positional", []string{"foo"}, 1},
		{"flag", []string{"--query", "foo"}, 1},
		{"flag and positional", []string{"--query", "a", "b"}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// No key anywhere: env empty, no keyring tool on PATH, empty config dir — so no network call.
			t.Setenv("TAVILY_API_KEY", "")
			t.Setenv("PATH", t.TempDir())
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			if got := Cmd(tc.args); got != tc.want {
				t.Fatalf("Cmd(%q) = %d, want %d", tc.args, got, tc.want)
			}
		})
	}
}

func TestOneShotExitCodes(t *testing.T) {
	ok := func(context.Context, string, int) (string, error) { return "x", nil }
	for name, tc := range map[string]struct {
		query   string
		max     int
		search  func(context.Context, string, int) (string, error)
		want    int
		wantOut string
	}{
		"success passes query and max_results": {"golang generics", 3, func(_ context.Context, q string, n int) (string, error) {
			if q != "golang generics" || n != 3 {
				t.Errorf("searcher got (%q, %d), want (golang generics, 3)", q, n)
			}
			return "the answer", nil
		}, 0, "the answer"},
		"empty query":    {"   ", 5, ok, 2, ""},
		"searcher error": {"q", 5, func(context.Context, string, int) (string, error) { return "", errors.New("boom") }, 1, ""},
		"zero max defaults to 5": {"q", 0, func(_ context.Context, _ string, n int) (string, error) {
			if n != 5 {
				t.Errorf("default max_results = %d, want 5", n)
			}
			return "ok", nil
		}, 0, "ok"},
	} {
		t.Run(name, func(t *testing.T) {
			var out strings.Builder
			if code := oneShot(context.Background(), tc.query, tc.max, tc.search, &out); code != tc.want {
				t.Errorf("exit = %d, want %d", code, tc.want)
			}
			if strings.TrimSpace(out.String()) != tc.wantOut {
				t.Errorf("out = %q, want %q", out.String(), tc.wantOut)
			}
		})
	}
}

func TestOneShotThreadsContextToSearcher(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var seen error
	search := func(c context.Context, _ string, _ int) (string, error) {
		seen = c.Err()
		return "ok", nil
	}
	if code := oneShot(ctx, "q", 5, search, io.Discard); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !errors.Is(seen, context.Canceled) {
		t.Errorf("searcher ctx.Err() = %v, want context.Canceled", seen)
	}
}

func closedTestOutput(t *testing.T) *os.File {
	t.Helper()
	out, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil || out.Close() != nil {
		t.Fatalf("closed output file: %v", err)
	}
	return out
}

func TestOneShotRejectsOutputWriteFailure(t *testing.T) {
	search := func(context.Context, string, int) (string, error) { return "the answer", nil }
	if code := oneShot(context.Background(), "golang", 3, search, closedTestOutput(t)); code == 0 {
		t.Fatal("closed output writer must return a nonzero exit code")
	}
}

func TestExtractCLIOutputBytes(t *testing.T) {
	want := "# https://example.com/a\nBODY\n\n" +
		"# http://127.0.0.1/\nfailed: refusing to fetch internal address 127.0.0.1\n\n"
	extract := func(context.Context, []string) (string, int, error) { return want, 1, nil }
	code, out, _ := hosttest.CaptureOutput(t, func() int {
		return extractURLs(context.Background(), []string{"https://example.com/a", "http://127.0.0.1/"}, extract)
	})
	if code != 0 || out != want {
		t.Errorf("code=%d out=%q, want %q", code, out, want)
	}
}

func TestExtractRejectsOutputWriteFailure(t *testing.T) {
	extract := func(context.Context, []string) (string, int, error) { return "# https://example.com/a\nBODY\n\n", 1, nil }
	hosttest.ClosedStdout(t)
	if code := extractURLs(context.Background(), []string{"https://example.com/a"}, extract); code == 0 {
		t.Fatal("closed stdout must return a nonzero exit code despite a successfully fetched page")
	}
}
