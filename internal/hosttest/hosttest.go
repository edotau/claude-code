// Package hosttest gives every test binary the same host: no agent CLIs on PATH, no inherited credentials,
// and HOME/XDG/CLAUDE_CONFIG_DIR in a throwaway dir. Imports stdlib only, so any package's tests may use it.
package hosttest

import (
	"crypto/rand"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scrubPrefixes name the inherited env a test must never see: vendor credentials, routing pins, and host markers.
var scrubPrefixes = []string{
	"CLAUDE", "HARNESS_", "ANTHROPIC_", "OPENAI_", "AZURE_OPENAI_", "DATABRICKS_", "GEMINI_", "GOOGLE_", "COPILOT_",
	"CODEX_", "OPENCODE_", "TAVILY_", "GH_", "GITHUB_", "WSL", "TMUX", "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE",
}

// Tools are the host tools the suite shells out to; vendor CLIs, tmux, tailscale and launchctl are deliberately absent.
var Tools = []string{
	"sh", "bash", "env", "cat", "cp", "mv", "rm", "mkdir", "ln", "ls", "chmod", "touch", "true", "false", "sleep",
	"echo", "printf", "test", "head", "tail", "grep", "sed", "awk", "sort", "tr", "wc", "dirname", "basename",
	"readlink", "uname", "id", "date", "tee", "xargs", "find", "diff", "stat", "mktemp", "kill", "ps", "pwd",
	"base64", "cut", "uniq", "rmdir", "realpath", "expr", "seq", "timeout", "tar", "gzip", "make", "git", "go", "gofmt", "cc", "gcc", "clang",
}

const activeEnv = "HOSTTEST_ACTIVE"

// Main runs m in the hermetic host and returns its exit code: os.Exit(hosttest.Main(m)).
func Main(m *testing.M) int {
	cleanup := Setup()
	code := m.Run()
	cleanup()
	return code
}

// Setup applies the hermetic host and returns the func that removes its temp dir; call it before m.Run.
func Setup() (cleanup func()) {
	if os.Getenv(activeEnv) != "" { // a re-exec'd test helper inherits its parent's host, including per-test overrides
		return func() {}
	}
	keepGo()
	root, err := os.MkdirTemp("", "hosttest-")
	if err != nil {
		panic(err)
	}
	if root, err = filepath.EvalSymlinks(root); err != nil {
		panic(err)
	}
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if scrubbed(name) {
			_ = os.Unsetenv(name)
		}
	}
	bin := filepath.Join(root, "bin")
	home := filepath.Join(root, "home")
	for _, d := range []string{bin, home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			panic(err)
		}
	}
	linkTools(bin)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	for k, v := range map[string]string{
		"PATH": bin, "HOME": home, "CLAUDE_CONFIG_DIR": filepath.Join(home, ".claude"),
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"), "XDG_DATA_HOME": filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME": filepath.Join(home, ".local", "state"), "XDG_CACHE_HOME": filepath.Join(home, ".cache"),
		"HARNESS_VAULT_KEY": base64.StdEncoding.EncodeToString(key),
	} {
		_ = os.Setenv(k, v)
	}
	_ = os.Setenv(activeEnv, root)
	return func() { _ = os.RemoveAll(root) }
}

func scrubbed(name string) bool {
	for _, p := range scrubPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// keepGo pins the Go toolchain locations while HOME still points at the real home, so nested `go` calls keep caches.
func keepGo() {
	home, _ := os.UserHomeDir()
	if os.Getenv("GOPATH") == "" && home != "" {
		_ = os.Setenv("GOPATH", filepath.Join(home, "go"))
	}
	if os.Getenv("GOMODCACHE") == "" {
		_ = os.Setenv("GOMODCACHE", filepath.Join(strings.Split(os.Getenv("GOPATH"), string(os.PathListSeparator))[0], "pkg", "mod"))
	}
	if cache, err := os.UserCacheDir(); err == nil && os.Getenv("GOCACHE") == "" {
		_ = os.Setenv("GOCACHE", filepath.Join(cache, "go-build"))
	}
	if os.Getenv("GOROOT") == "" {
		if out, err := exec.Command("go", "env", "GOROOT").Output(); err == nil {
			_ = os.Setenv("GOROOT", strings.TrimSpace(string(out)))
		}
	}
}

func linkTools(bin string) {
	for _, t := range Tools {
		p, err := exec.LookPath(t)
		if err != nil {
			continue
		}
		if real, err := filepath.EvalSymlinks(p); err == nil && filepath.Base(real) == t {
			p = real
		}
		_ = os.Symlink(p, filepath.Join(bin, t))
	}
}
