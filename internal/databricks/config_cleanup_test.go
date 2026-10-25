package databricks

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestEnvOrDefault(t *testing.T) {
	cases := []struct{ env, provider, want string }{
		{"", "", "qa"},
		{"", "databricks-dev", "dev"},
		{"", "databricks-prod-oai", "prod"},
		{"", "databricks-staging", "qa"},
		{"", "openai", "qa"},
		{"prod", "databricks-dev", "prod"},
	}
	for _, c := range cases {
		t.Setenv("HARNESS_PROVIDER", c.provider)
		if got := envOrDefault(c.env); got != c.want {
			t.Errorf("env=%q HARNESS_PROVIDER=%q: got %q, want %q", c.env, c.provider, got, c.want)
		}
	}
}

// rescueScratch runs rescue with a recording spawn; it returns the scratch path and whether it existed mid-run.
func rescueScratch(t *testing.T, spawnErr error) (path string, existedDuring bool, code int) {
	t.Helper()
	fakeClaudeOnPath(t)
	old := spawn
	spawn = func(_ string, argv, _ []string, _ io.Reader, _, _ io.Writer) (int, error) {
		path = argValue(argv, "--settings")
		_, err := os.Stat(path)
		existedDuring = err == nil
		return 3, spawnErr
	}
	t.Cleanup(func() { spawn = old })
	code, _, _ = runCaptured(t, func() int { return rescue([]string{"--host", exHost}) })
	t.Cleanup(func() { os.Remove(path) })
	return path, existedDuring, code
}

func TestRescueDeletesScratchAfterChildExits(t *testing.T) {
	path, during, code := rescueScratch(t, nil)
	if code != 3 || path == "" || !during {
		t.Fatalf("code %d path %q existedDuring %v", code, path, during)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("scratch survived the child: %v", err)
	}
}

func TestRescueDeletesScratchWhenSpawnFails(t *testing.T) {
	path, _, code := rescueScratch(t, errors.New("boom"))
	if code != 2 || path == "" {
		t.Fatalf("code %d path %q", code, path)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("scratch survived a failed spawn: %v", err)
	}
}

func TestRescuePrintKeepsScratch(t *testing.T) {
	fakeClaudeOnPath(t)
	f := &fakeChild{}
	useFake(t, f)
	_, out, _ := runCaptured(t, func() int { return rescue([]string{"--host", exHost, "--print"}) })
	i := strings.Index(out, "claude --settings ")
	if i < 0 {
		t.Fatalf("no command line:\n%s", out)
	}
	file := strings.Fields(out[i:])[2]
	t.Cleanup(func() { os.Remove(file) })
	if _, err := os.Stat(file); err != nil {
		t.Errorf("--print removed the scratch file: %v", err)
	}
}
