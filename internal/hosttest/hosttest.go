// Package hosttest gives every test binary the same host: no agent CLIs on PATH, no inherited credentials,
// and HOME/XDG/CLAUDE_CONFIG_DIR in a throwaway dir. Imports stdlib (plus x/sys/unix in the pty files),
// so any package's tests may use it.
package hosttest

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// scrubPrefixes name the inherited env a test must never see: agent credentials, routing pins, and host markers.
var scrubPrefixes = []string{
	"CLAUDE", "HARNESS_", "ANTHROPIC_", "OPENAI_", "AZURE_OPENAI_", "DATABRICKS_", "GEMINI_", "GOOGLE_", "COPILOT_",
	"CODEX_", "OPENCODE_", "TAVILY_", "GH_", "GITHUB_", "WSL", "TMUX", "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE",
	"AGENT_ERROR_FALLBACK",
}

// Tools are the host tools the suite shells out to; agent CLIs, tmux, tailscale and launchctl are deliberately absent.
var Tools = []string{
	"sh", "bash", "env", "cat", "cp", "mv", "rm", "mkdir", "ln", "ls", "chmod", "touch", "true", "false", "sleep",
	"echo", "printf", "test", "head", "tail", "grep", "sed", "awk", "sort", "tr", "wc", "dirname", "basename",
	"readlink", "uname", "id", "date", "tee", "xargs", "find", "diff", "stat", "mktemp", "kill", "ps", "lsof", "pwd",
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
	root := makeRoot()
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
	setHostEnv(bin, home)
	_ = os.Setenv(activeEnv, root)
	return func() { _ = os.RemoveAll(root) }
}

// makeRoot creates the host's temp dir with symlinks resolved.
func makeRoot() string {
	root, err := os.MkdirTemp("", "hosttest-")
	if err != nil {
		panic(err)
	}
	if root, err = filepath.EvalSymlinks(root); err != nil {
		panic(err)
	}
	return root
}

// setHostEnv points PATH, HOME, the XDG dirs, the vault key and git's config at the hermetic host.
func setHostEnv(bin, home string) {
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
	for k, v := range map[string]string{ // git skips system/global config, prompts, optional locks and auto maintenance
		"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.DevNull, "GIT_TERMINAL_PROMPT": "0", "GIT_OPTIONAL_LOCKS": "0",
		"GIT_CONFIG_COUNT": "2", "GIT_CONFIG_KEY_0": "maintenance.auto", "GIT_CONFIG_VALUE_0": "false",
		"GIT_CONFIG_KEY_1": "gc.auto", "GIT_CONFIG_VALUE_1": "0",
	} {
		_ = os.Setenv(k, v)
	}
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
		if out, err := exec.CommandContext(context.Background(), "go", "env", "GOROOT").Output(); err == nil {
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

// HermeticEnv points CLAUDE_CONFIG_DIR, HOME and XDG config/data at a fresh temp dir and clears model/key overrides.
func HermeticEnv(t testing.TB) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	home := filepath.Join(dir, "home")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("BASH_COMPLETION_USER_DIR", "")
	for _, k := range []string{"CLAUDE_PROFILE", "CLAUDE_MODEL", "CLAUDE_OPUS_MODEL", "CLAUDE_SONNET_MODEL",
		"CLAUDE_HAIKU_MODEL", "CLAUDE_FABLE_MODEL", "ANTHROPIC_API_KEY", "TEST_GW_KEY"} {
		t.Setenv(k, "")
	}
}

// TierHarness points HOME/CLAUDE_CONFIG_DIR at a temp config tree and neutralizes an ambient vendor env.
func TierHarness(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	harness := filepath.Join(home, ".claude")
	if err := os.MkdirAll(harness, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", harness)
	t.Setenv("HARNESS_HOOK_AGENT", "")
	return harness
}

// BashPayload is a Claude-shaped Bash hook payload for cmd.
func BashPayload(cmd string) *strings.Reader {
	b, _ := json.Marshal(map[string]any{"tool_name": "Bash", "tool_input": map[string]string{"command": cmd}})
	return strings.NewReader(string(b))
}

// RTKBashIn is BashPayload's JSON with the description and timeout fields an rtk rewrite must carry over.
func RTKBashIn(cmd string) string {
	b, _ := json.Marshal(map[string]any{
		"tool_name":  "Bash",
		"tool_input": map[string]any{"command": cmd, "description": "d", "timeout": 120},
	})
	return string(b)
}

// fakeScript writes an executable script called name into a fresh temp dir and returns its path.
func fakeScript(t *testing.T, name, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	WriteExec(t, p, script)
	return p
}

// WriteExec writes body as an executable file at path, creating parent directories.
func WriteExec(t *testing.T, path, body string) {
	t.Helper()
	WriteRel(t, filepath.Dir(path), filepath.Base(path), body)
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// SymlinkLoop makes path a self-referencing symlink: a deterministic non-ENOENT stat failure, even as root.
func SymlinkLoop(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("a self-referencing symlink (ELOOP) needs unix")
	}
	if err := os.Symlink(path, path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("fixture stat = %v, want ELOOP", err)
	}
}

// UnsignedJWT is an unsigned three-part token whose payload is the given JSON claims.
func UnsignedJWT(claims string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(claims)) + ".sig"
}

// CountFiles counts the files in dir whose name starts with prefix and whose trimmed content is want.
func CountFiles(dir, prefix, want string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil && strings.TrimSpace(string(b)) == want {
			n++
		}
	}
	return n
}

// FakeRTK installs a stub rtk (HARNESS_RTK_BIN) whose `rewrite <cmd>` prints FAKE_RTK_OUT or "rtk <cmd>".
func FakeRTK(t *testing.T) string {
	t.Helper()
	bin := fakeScript(t, "rtk", "#!/bin/sh\n[ \"$1\" = rewrite ] || exit 9\n"+
		"if [ -n \"$FAKE_RTK_OUT\" ]; then printf '%s' \"$FAKE_RTK_OUT\"; else printf '%s' \"rtk $2\"; fi\n"+
		"exit ${FAKE_RTK_EXIT:-0}\n")
	t.Setenv("HARNESS_RTK_BIN", bin) // paths.RTKBinaryEnv; this package imports stdlib + x/sys/unix
	return bin
}

// FakeOpencode puts an `opencode` script running body first on PATH and returns a directory to use as cwd.
func FakeOpencode(t *testing.T, body string) string {
	t.Helper()
	dir := filepath.Dir(fakeScript(t, "opencode", "#!/bin/sh\n"+body+"\n"))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

// WriteEdits writes a Claude transcript of n Edit tool calls at path.
func WriteEdits(t *testing.T, path string, n int) {
	t.Helper()
	line := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/r/a.go"}}]}}` + "\n"
	WriteRel(t, filepath.Dir(path), filepath.Base(path), strings.Repeat(line, n))
}

// WriteUserTurns writes a Claude transcript of n user turns at path.
func WriteUserTurns(t *testing.T, path string, n int) {
	t.Helper()
	WriteRel(t, filepath.Dir(path), filepath.Base(path), strings.Repeat(`{"type":"user","message":{"content":"ship it"}}`+"\n", n))
}

// MainTB lets TestMain call testing.TB stubs: Cleanup is deferred until the caller runs Run's returned funcs.
type MainTB struct {
	testing.TB
	Cleanups []func()
}

func (m *MainTB) Helper()          {}
func (m *MainTB) Cleanup(f func()) { m.Cleanups = append(m.Cleanups, f) }

// OneDoc decodes s as exactly one JSON object, failing the test on anything else.
func OneDoc(t *testing.T, s string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("not JSON: %q: %v", s, err)
	}
	if _, err := dec.Token(); err != io.EOF { // a stray closer ('}' or ']') yields a token, not EOF
		t.Fatalf("more than one JSON document: %q", s)
	}
	return m
}
