package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/network/tailscale"
	"github.com/edotau/claude-code/internal/proc/proctest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/tui"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaultdoc"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
	"go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

// Usage errors exit 2 before any write, print nothing to stdout and name the problem on stderr.
func TestCmdVault(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		want   int
		stderr string
	}{
		{"nil", nil, 0, ""},
		{"short help", []string{"-h"}, 0, ""},
		{"long help", []string{"--help"}, 0, ""},
		{"unknown", []string{"nope"}, 2, ""},
		{"ls unknown section", []string{"ls", "bogus"}, 2, "bogus"},
		{"ls unknown format", []string{"ls", "--format", "bogus"}, 2, `"bogus"`},
		{"edit extra arg", []string{"edit", "x"}, 2, ""},
		{"ls", []string{"ls"}, 0, ""},
		{"show removed", []string{"show"}, 2, ""},
		{"list renamed to ls", []string{"list"}, 2, ""},
		{"finalize is unknown", []string{"finalize"}, 2, ""},
		{"rollback is unknown", []string{"rollback"}, 2, ""},
		{"rotate-secret extra arg", []string{"rotate-secret", "x"}, 2, ""},
		{"rotate-key removed", []string{"rotate-key"}, 2, ""},
		{"key removed", []string{"key", "show"}, 2, ""},
		{"set bare", []string{"set"}, 2, "usage: claude-code vault set [NAME]"},
		{"set NAME --tailscale", []string{"set", "VAULT_TEST_DUMMY", "--tailscale"}, 2, ""},
		{"export to stdout", []string{"export", "--out", "-"}, 2, "vault get"},
		{"get --key with a name", []string{"get", "--key", "ALPHA"}, 2, ""},
		{"get --key --export", []string{"get", "--key", "--export"}, 2, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vaulttest.UseFreshKey(t)
			vaulttest.Secret(t, "ALPHA", "obviously-fake-alpha")
			withStdin(t, "obviously-fake-unused\n")
			code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdVault(tt.args) })
			if code != tt.want || (code == 2 && stdout != "") || !strings.Contains(stderr, tt.stderr) {
				t.Errorf("cmdVault(%q) = %d, stdout %q, stderr %q; want %d naming %q", tt.args, code, stdout, stderr, tt.want, tt.stderr)
			}
			if names := vault.Names(); !slices.Equal(names, []string{"ALPHA"}) {
				t.Errorf("cmdVault(%q) changed the stored names: %v", tt.args, names)
			}
		})
	}
}

// Bare `vault` (and a leading flag) prints the ls table, not the help page.
func TestCmdVaultBareIsLs(t *testing.T) {
	vaulttest.UseFreshKey(t)
	for _, tc := range []struct {
		args []string
		want string
	}{{nil, `"secrets"`}, {[]string{"--format", "text"}, "AGENT"}} { // piped bare defaults to JSON
		code, stdout, _ := hosttest.CaptureOutput(t, func() int { return cmdVault(tc.args) })
		if code != 0 || strings.Contains(stdout, "COMMANDS") || !strings.Contains(stdout, tc.want) {
			t.Errorf("vault %q = %d, want the ls table:\n%s", tc.args, code, stdout)
		}
	}
}

// vault edit has no flags or commands of its own; -h must print its own usage-only page, not vaultPage's
// COMMANDS list (every other vault -h path already covers the parent page itself).
func TestCmdVaultEditHelpPrintsOwnPage(t *testing.T) {
	vaulttest.UseFreshKey(t)
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdVault([]string{"edit", "-h"}) })
	if code != 0 {
		t.Fatalf("vault edit -h: exit %d, want 0", code)
	}
	got := stdout + stderr
	if !strings.HasPrefix(got, "usage: claude-code vault edit\n") {
		t.Errorf("vault edit -h: want its own usage line first:\n%s", got)
	}
	if strings.Contains(got, "COMMANDS") || strings.Contains(got, "claude-code vault <command>") {
		t.Errorf("vault edit -h: leaked the parent vaultPage:\n%s", got)
	}
}

// The dispatch table and the completion spec name the same vault subcommands.
func TestVaultVerbs(t *testing.T) {
	var verbs, spec []string
	for name := range vaultVerbs {
		verbs = append(verbs, name)
	}
	for name := range specTree()["vault"].Sub {
		spec = append(spec, name)
	}
	slices.Sort(verbs)
	slices.Sort(spec)
	if len(verbs) == 0 || !slices.Equal(verbs, spec) {
		t.Errorf("vaultVerbs = %v, vault spec subwords = %v; want the same set", verbs, spec)
	}
}

func vaultTestStdin(t *testing.T, input string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	t.Cleanup(func() {
		os.Stdin = old
		f.Close()
	})
	if _, err := f.WriteString(input); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	os.Stdin = f
}

func TestCmdVaultListTailscalePresenceOnly(t *testing.T) {
	vaulttest.UseFreshKey(t)
	for _, name := range []string{
		"TAILSCALE_OAUTH_CLIENT_ID", "TAILSCALE_OAUTH_CLIENT_SECRET",
		"TAILSCALE_API_KEY", "TAILSCALE_AUTHKEY",
	} {
		t.Setenv(name, "")
	}
	const value = "known-tailscale-api-key-value"
	if err := vault.Store("TAILSCALE_API_KEY", value); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
		return cmdVault([]string{"ls", "--tailscale"})
	})
	if code != 0 {
		t.Fatalf("vault ls --tailscale = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "TAILSCALE_API_KEY  set (") {
		t.Errorf("stdout = %q, want stored credential presence", stdout)
	}
	if strings.Contains(stdout, value) {
		t.Error("stdout contains the raw secret value")
	}
	if strings.Contains(stdout, "chars)") {
		t.Error("stdout contains a secret character count")
	}
	want := "TAILSCALE_OAUTH_CLIENT_ID  missing\n" +
		"TAILSCALE_OAUTH_CLIENT_SECRET  missing\n" +
		"TAILSCALE_API_KEY  set (vault)\n" +
		"TAILSCALE_AUTHKEY  missing\n" +
		"api: api-key\n"
	if stdout != want {
		t.Errorf("stdout = %q, want presence-only output %q", stdout, want)
	}
}

func TestCmdVaultSetTailscaleStoresPipedLines(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		args        []string
		want        map[string]string
	}{
		{"oauth pair", "id-value\nsecret-value\n", []string{"set", "--tailscale"},
			map[string]string{"TAILSCALE_OAUTH_CLIENT_ID": "id-value", "TAILSCALE_OAUTH_CLIENT_SECRET": "secret-value"}},
		{"api key", "apikeyvalue\n", []string{"set", "--tailscale", "--api-key"},
			map[string]string{"TAILSCALE_API_KEY": "apikeyvalue"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vaulttest.UseFreshKey(t)
			vaultTestStdin(t, tc.input)
			code, _, stderr := hosttest.CaptureOutput(t, func() int { return cmdVault(tc.args) })
			if code != 0 {
				t.Fatalf("vault %q = %d, stderr = %q", tc.args, code, stderr)
			}
			for name, want := range tc.want {
				value, source, ok := vault.Get(name)
				if !ok || vault.Redact(value) != vault.Redact(want) || source != "vault" {
					t.Errorf("%s was not stored correctly in the vault", name)
				}
			}
		})
	}
}

func TestCmdVaultSetTailscaleEmptyValueFails(t *testing.T) {
	vaulttest.UseFreshKey(t)
	for _, tc := range []struct {
		name  string
		input string
	}{
		{"both empty", "\n\n"},
		{"empty id", "\nsecret-value\n"},
		{"empty secret", "id-value\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vaulttest.UseFreshKey(t)
			vaultTestStdin(t, tc.input)
			code, _, stderr := hosttest.CaptureOutput(t, func() int {
				return cmdVault([]string{"set", "--tailscale"})
			})
			if code != 1 {
				t.Fatalf("vault set --tailscale = %d, want 1; stderr = %q", code, stderr)
			}
			names := vault.Names()
			for _, name := range []string{"TAILSCALE_OAUTH_CLIENT_ID", "TAILSCALE_OAUTH_CLIENT_SECRET"} {
				if slices.Contains(names, name) {
					t.Errorf("empty value stored %s", name)
				}
			}
		})
	}
}

func TestVaultSetDryRunStoresNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		input []string
		want  []string
	}{
		{"tailscale", []string{"set", "--tailscale", "--dry-run"}, []string{"id-value", "secret-value"},
			[]string{"would add TAILSCALE_OAUTH_CLIENT_ID (8 chars)", "would add TAILSCALE_OAUTH_CLIENT_SECRET (12 chars)"}},
		{"name", []string{"set", "VAULT_TEST_DUMMY", "--dry-run"}, []string{"obviously-fake-dry-run"},
			[]string{"would add VAULT_TEST_DUMMY"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vaulttest.UseFreshKey(t)
			vaultTestStdin(t, strings.Join(tc.input, "\n")+"\n")
			code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdVault(tc.args) })
			if code != 0 {
				t.Fatalf("vault %q = %d, stderr = %q", tc.args, code, stderr)
			}
			for _, want := range tc.want {
				if !strings.Contains(stdout, want) {
					t.Errorf("stdout = %q, want %q", stdout, want)
				}
			}
			for _, value := range tc.input {
				if strings.Contains(stdout+stderr, value) {
					t.Errorf("dry run leaked the raw value %q", value)
				}
			}
			if names := vault.Names(); len(names) != 0 {
				t.Errorf("dry run stored %v", names)
			}
		})
	}
}

// A value spanning lines (embedded \r or \n) must be refused before any write, even when it is
// the second of a pair — vault.Store would reject it anyway, but half a pair must never land.
func TestVaultStoreTailscaleRefusesEmbeddedNewline(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pairs []tailscale.Cred
	}{
		{"carriage return in first", []tailscale.Cred{
			{Name: "TAILSCALE_OAUTH_CLIENT_ID", Value: "id-\rvalue"},
			{Name: "TAILSCALE_OAUTH_CLIENT_SECRET", Value: "secret-value"},
		}},
		{"newline in second", []tailscale.Cred{
			{Name: "TAILSCALE_OAUTH_CLIENT_ID", Value: "id-value"},
			{Name: "TAILSCALE_OAUTH_CLIENT_SECRET", Value: "secret-\nvalue"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vaulttest.UseFreshKey(t)
			code, _, stderr := hosttest.CaptureOutput(t, func() int {
				return vaultStoreTailscale(false, tc.pairs)
			})
			if code != 1 {
				t.Fatalf("vaultStoreTailscale = %d, want 1; stderr = %q", code, stderr)
			}
			if !strings.Contains(stderr, "spans lines; nothing written") {
				t.Errorf("stderr = %q, want the spans-lines refusal", stderr)
			}
			names := vault.Names()
			for _, p := range tc.pairs {
				if slices.Contains(names, p.Name) {
					t.Errorf("a value spanning lines stored %s even though it was refused", p.Name)
				}
			}
		})
	}
}

func TestCmdVaultRotateSecretRefusesWithEnvSetWithoutForce(t *testing.T) {
	dir, _ := vaulttest.UseKeyring(t)
	_ = dir
	if _, err := vault.EnsureKey(); err != nil {
		t.Fatal(err)
	}
	// A key_command (rather than env) is simplest to set up at this layer: present without
	// --force refuses, present with --force rotates.
	vaulttest.Put(t, vault.Meta, "key_command", `["/bin/true"]`)

	code, _, stderr := hosttest.CaptureOutput(t, func() int { return cmdVault([]string{"rotate-secret"}) })
	if code != 1 {
		t.Fatalf("vault rotate-secret = %d, want 1; stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "key_command") {
		t.Errorf("stderr = %q, want it to name key_command", stderr)
	}

	code, _, stderr = hosttest.CaptureOutput(t, func() int { return cmdVault([]string{"rotate-secret", "--force"}) })
	if code != 0 {
		t.Fatalf("vault rotate-secret --force = %d, want 0; stderr = %q", code, stderr)
	}
}

// A value of the wrong shape for its name is refused and never stored.
func TestVaultSetRefusesMalformedShape(t *testing.T) {
	for _, tc := range []struct{ name, input, refusal string }{
		{"COPILOT_GITHUB_TOKEN", "ok\n", "not a GitHub token"},
		{"TAILSCALE_OAUTH_CLIENT_ID", "not-a-client-id\n", "want a client ID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vaulttest.UseFreshKey(t)
			withStdin(t, tc.input)
			code, _, stderr := hosttest.CaptureOutput(t, func() int { return cmdVault([]string{"set", tc.name}) })
			if code == 0 || !strings.Contains(stderr, tc.refusal) {
				t.Fatalf("set %s: exit %d, stderr = %q, want the %q refusal", tc.name, code, stderr, tc.refusal)
			}
			if _, _, ok := vault.Get(tc.name); ok {
				t.Fatal("a refused value must not be stored")
			}
		})
	}
}

// A valid-shaped GitHub token is accepted and sealed in the vault.
func TestVaultSetCopilotTokenAcceptsValidShape(t *testing.T) {
	vaulttest.UseFreshKey(t)
	const tok = "gh" + "p_validshapedFAKEtoken1234567890"
	withStdin(t, tok+"\n")

	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdVault([]string{"set", "COPILOT_GITHUB_TOKEN"}) })
	if code != 0 {
		t.Fatalf("set COPILOT_GITHUB_TOKEN exit = %d", code)
	}
	if v, _, ok := vault.Get("COPILOT_GITHUB_TOKEN"); !ok || vault.Redact(v) != vault.Redact(tok) {
		t.Fatal("COPILOT_GITHUB_TOKEN was not stored correctly")
	}
	if strings.Contains(stdout+stderr, tok) {
		t.Fatal("set leaked the raw value")
	}
	if !strings.Contains(stderr, vault.Redact(tok)) {
		t.Fatal("set did not report the masked value")
	}
}

func withStdin(t *testing.T, content string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(content); err != nil {
		t.Fatal(err)
	}
	w.Close()
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = old
		r.Close()
	})
}

func TestVaultStorageHelpPrintsUsage(t *testing.T) {
	vaulttest.UseFreshKey(t)
	for _, op := range []string{"set", "get", "delete", "import", "export"} {
		code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdVault([]string{op, "-h"}) })
		if code != 0 || !strings.Contains(stderr, "usage: claude-code vault "+op) || stdout != "" {
			t.Errorf("vault %s -h: want usage + exit 0 and no op output, got exit %d", op, code)
		}
	}
}

// assertGetKeyRefusesTTY runs `vault get --key` with slave (a pty slave) as stdout and expects the refusal.
func assertGetKeyRefusesTTY(t *testing.T, slave *os.File) {
	t.Helper()
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
		old := os.Stdout
		os.Stdout = slave
		defer func() { os.Stdout = old }()
		return cmdVault([]string{"get", "--key"})
	})
	if code != 2 || stdout != "" || !strings.Contains(stderr, "refusing") {
		t.Fatalf("get --key on TTY: exit %d, stdout empty=%v, refusal=%v",
			code, stdout == "", strings.Contains(stderr, "refusing"))
	}
}

func TestCmdVaultGetKeyNonTTYDecodesToMasterKey(t *testing.T) {
	vaulttest.UseFreshKey(t)
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdVault([]string{"get", "--key"}) })
	if code != 0 {
		t.Fatalf("vault get --key = %d, want 0; stderr empty=%v", code, stderr == "")
	}
	got, err := base64.StdEncoding.DecodeString(strings.TrimSpace(stdout))
	if err != nil {
		t.Fatal("vault get --key stdout does not decode as base64")
	}
	if len(got) != 32 {
		t.Errorf("decoded master key is %d bytes, want 32", len(got))
	}
	want, err := base64.StdEncoding.DecodeString(os.Getenv("HARNESS_VAULT_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Error("vault get --key printed a different key than HARNESS_VAULT_KEY")
	}
}

func TestVaultDeleteStoredSecret(t *testing.T) {
	vaulttest.UseFreshKey(t)
	const value = "obviously-fake-delete"
	if err := vault.Store("VAULT_TEST_DUMMY", value); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
		return cmdVault([]string{"delete", "VAULT_TEST_DUMMY"})
	})
	if code != 0 || len(vault.Names()) != 0 {
		t.Fatalf("delete: exit %d, stored names=%v", code, vault.Names())
	}
	if strings.Contains(stdout+stderr, value) {
		t.Fatal("delete leaked the raw value")
	}
}

func TestVaultExportFileOnly(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "out"}[explicit], func(t *testing.T) {
			vaulttest.UseFreshKey(t)
			dir := t.TempDir()
			t.Chdir(dir)
			const value = "obviously-fake-alpha"
			if err := vault.Store("ALPHA", value); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, ".env")
			args := []string{"export"}
			if explicit {
				path = filepath.Join(dir, "export.env")
				args = append(args, "--out", path)
			}
			for range 2 {
				code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdVault(args) })
				if code != 0 || stdout != "" || strings.Contains(stderr, value) {
					t.Fatalf("export: exit %d, stdout empty=%v, leaked=%v",
						code, stdout == "", strings.Contains(stderr, value))
				}
			}
			checkExportedFile(t, path, value)
		})
	}
}

// checkExportedFile asserts path is 0600 and holds exactly one ALPHA row whose masked value matches value's.
func checkExportedFile(t *testing.T, path, value string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("export permissions = %o, want 600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pairs, problems := vault.ParseDotenv(strings.NewReader(string(data)))
	if len(problems) != 0 || len(pairs) != 1 || pairs[0].Name != "ALPHA" {
		t.Fatal("export did not preserve exactly one masked NAME=value row")
	}
	if vault.Redact(pairs[0].Value) != vault.Redact(value) {
		t.Fatal("export changed the masked value")
	}
}

// import - stores each valid piped NAME=value with --yes, warns per bad line by name; no --yes or nothing valid: none.
func TestVaultImportDashFromPipe(t *testing.T) {
	for _, tc := range []struct {
		name, refusal string
		args          []string
		pairs         [][2]string
		extra, warn   string
	}{
		{"without --yes", "--yes", []string{"import", "-"}, [][2]string{{"GAMMA", "obviously-fake-gamma"}}, "", ""},
		{"malformed sibling", "", []string{"import", "-", "--yes"},
			[][2]string{{"GAMMA", "obviously-fake-gamma"}}, "not-a-pair\n", "warning: unable to parse line 2: want NAME=value"},
		{"bad token sibling", "", []string{"import", "-", "--yes"},
			[][2]string{{"GAMMA", "obviously-fake-gamma"}}, "GITHUB_WORK_PAT=not-a-token\n", "unable to parse GITHUB_WORK_PAT: "},
		{"bad GitHub token", "not a GitHub token", []string{"import", "-", "--yes"},
			[][2]string{{"GITHUB_WORK_PAT", "not-a-token"}}, "", "nothing written"},
		{"with --yes", "", []string{"import", "-", "--yes"},
			[][2]string{{"GAMMA", "obviously-fake-gamma"}, {"DELTA", "obviously-fake-delta"}}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vaulttest.UseFreshKey(t)
			var input strings.Builder
			for _, p := range tc.pairs {
				input.WriteString(p[0] + "=" + p[1] + "\n")
			}
			withStdin(t, input.String()+tc.extra)
			code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdVault(tc.args) })
			if (code == 0) != (tc.refusal == "") || !strings.Contains(stderr, tc.refusal) || !strings.Contains(stderr, tc.warn) {
				t.Fatalf("vault %q: exit %d, stderr = %q, want refusal %q, warning %q", tc.args, code, stderr, tc.refusal, tc.warn)
			}
			checkImported(t, tc.pairs, tc.refusal == "", stdout+stderr)
			if _, v, ok := strings.Cut(strings.TrimSpace(tc.extra), "="); ok && strings.Contains(stdout+stderr, v) {
				t.Error("import printed a skipped value")
			}
		})
	}
}

// checkImported asserts each pair is sealed in the vault (or absent) and its raw value never printed.
func checkImported(t *testing.T, pairs [][2]string, stored bool, output string) {
	t.Helper()
	for _, p := range pairs {
		got, source, err := vault.Lookup(p[0])
		if stored && (err != nil || source != "vault" || vault.Redact(got) != vault.Redact(p[1])) {
			t.Errorf("%s was not imported into the vault correctly", p[0])
		}
		if !stored && got != "" {
			t.Errorf("%s stored despite the refusal", p[0])
		}
		if strings.Contains(output, p[1]) {
			t.Errorf("import leaked %s", p[0])
		}
	}
}

func TestVaultGetMultipleNames(t *testing.T) {
	for _, export := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "export"}[export], func(t *testing.T) {
			vaulttest.UseFreshKey(t)
			values := []string{"obviously-fake-alpha", "obviously-fake-beta's-value"}
			for i, name := range []string{"ALPHA", "BETA"} {
				if err := vault.Store(name, values[i]); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"get", "ALPHA", "BETA"}
			if export {
				args = append(args, "--export")
			}
			code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdVault(args) })
			if code != 0 || stderr != "" {
				t.Fatalf("get: exit %d, stderr empty=%v", code, stderr == "")
			}
			if got, want := maskGetOutput(stdout, []string{"ALPHA", "BETA"}, values, export); got != want {
				t.Fatal("get did not produce the expected masked NAME=value rows")
			}
		})
	}
}

// maskGetOutput redacts each value in stdout (shell-quoted with export) and builds the masked rows get should print.
func maskGetOutput(stdout string, names, values []string, export bool) (got, want string) {
	var wantSb strings.Builder
	for i, name := range names {
		value := values[i]
		if export {
			value = textutil.ShellQuote(value)
		}
		stdout = strings.ReplaceAll(stdout, value, vault.Redact(values[i]))
		prefix := ""
		if export {
			prefix = "export "
		}
		wantSb.WriteString(prefix)
		wantSb.WriteString(name)
		wantSb.WriteByte('=')
		wantSb.WriteString(vault.Redact(values[i]))
		wantSb.WriteByte('\n')
	}
	return stdout, wantSb.String()
}

func TestVaultGetMissingNamePrintsNothing(t *testing.T) {
	vaulttest.UseFreshKey(t)
	t.Setenv("ABSENT_VAULT_TEST_KEY", "")
	if err := vault.Store("ALPHA", "obviously-fake-alpha"); err != nil {
		t.Fatal(err)
	}
	code, stdout, _ := hosttest.CaptureOutput(t, func() int {
		return cmdVault([]string{"get", "ALPHA", "ABSENT_VAULT_TEST_KEY"})
	})
	if code != 1 || stdout != "" {
		t.Fatalf("missing get: exit %d, stdout empty=%v", code, stdout == "")
	}
}

func TestVaultListSections(t *testing.T) {
	vaulttest.UseFreshKey(t)
	seedVaultListFixture(t)
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			run := listRunner(t, format)
			bare := run()
			if bare != run("all") {
				t.Error("all differs from bare list")
			}
			filtered := run("PINS", "SeCrEtS", "pins")
			if format == "json" {
				checkFilteredJSON(t, filtered, run("pins"))
				return
			}
			checkFilteredText(t, bare, filtered)
		})
	}
	for _, names := range [][]string{{"bogus"}, {"all", "bogus"}, {"bogus", "all"}} {
		code, _, stderr := hosttest.CaptureOutput(t, func() int { return cmdVault(append([]string{"ls"}, names...)) })
		if code != 2 || !strings.Contains(stderr, "bogus") {
			t.Errorf("unknown section %v: exit %d, missing invalid-name hint=%v",
				names, code, !strings.Contains(stderr, "bogus"))
		}
	}
}

// listRunner runs `vault ls --format format <names>`, failing on a non-zero exit or a leaked raw value.
func listRunner(t *testing.T, format string) func(names ...string) string {
	return func(names ...string) string {
		t.Helper()
		args := append([]string{"ls", "--format", format}, names...)
		code, stdout, _ := hosttest.CaptureOutput(t, func() int { return cmdVault(args) })
		if code != 0 {
			t.Fatalf("list sections %v exit %d", names, code)
		}
		if strings.Contains(stdout, "tvly-dev-secret-leak-me-not") {
			t.Fatal("list leaked a raw value")
		}
		return stdout
	}
}

// checkFilteredJSON asserts the secrets+pins selection holds exactly those keys and pins-only omits secrets.
func checkFilteredJSON(t *testing.T, filtered, pinsOnly string) {
	t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(filtered), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc) != 2 || doc["secrets"] == nil || doc["pins"] == nil {
		t.Error("filtered JSON must contain only secrets and pins")
	}
	doc = nil
	if err := json.Unmarshal([]byte(pinsOnly), &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["secrets"]; ok {
		t.Error("pins-only JSON includes secrets")
	}
}

// checkFilteredText asserts bare text lists sections in canonical order and the filtered text keeps only SECRETS and PINS.
func checkFilteredText(t *testing.T, bare, filtered string) {
	t.Helper()
	var headings []string
	for line := range strings.SplitSeq(bare, "\n") {
		if slices.Contains([]string{"SECRETS", "PINS", "ENV", "PROVIDERS", "SETTINGS", "VAULT"}, line) {
			headings = append(headings, line)
		}
	}
	if !slices.Equal(headings, []string{"SECRETS", "PINS", "ENV", "PROVIDERS", "SETTINGS", "VAULT"}) {
		t.Errorf("section order = %v", headings)
	}
	if !strings.HasPrefix(filtered, "SECRETS\n") || !strings.Contains(filtered, "\nPINS\n") {
		t.Error("filtered sections must retain canonical order")
	}
	for _, head := range []string{"ENV", "PROVIDERS", "SETTINGS", "VAULT"} {
		if strings.Contains(filtered, "\n"+head+"\n") {
			t.Errorf("filtered text includes %s", head)
		}
	}
}

func TestVaultListProviderAndVendorTables(t *testing.T) {
	vaulttest.UseFreshKey(t)
	t.Setenv("COLUMNS", "200")
	vaulttest.Put(t, vault.Overlay, "providers", `{
		"providers":{"openai":{"fake":{"auth":{"type":"none"},"models":{"sonnet":"fake-sonnet"}}}},
		"agentPlans":{"copilot":[{"tier":"standard","id":"fake-plan"}]},
		"routing":{"opencode":{"ladder":["one","two"]}}}`)
	vaulttest.Put(t, vault.Overlay, "agents", `{"agents":{"opencode":{
		"_comment":"omit-this-comment","launch":{"env":{"FAKE_ENV":"enabled"},"args":["--fake"]},
		"future":{"enabled":true}}}}`)
	code, stdout, _ := hosttest.CaptureOutput(t, func() int {
		return cmdVault([]string{"ls", "providers", "routing", "agents", "--format", "text"})
	})
	if code != 0 {
		t.Fatalf("list tables exit %d", code)
	}
	normalized := strings.Join(strings.Fields(stdout), " ")
	for _, want := range []string{
		"NAME PROVIDER AUTH SONNET", "fake openai none fake-sonnet",
		"ROUTING", "opencode.ladder[0] one", "opencode.ladder[1] two",
		"launch.env.FAKE_ENV enabled", "launch.args --fake", "future.enabled true",
	} {
		if !strings.Contains(normalized, want) {
			t.Errorf("missing table content %q", want)
		}
	}
	if strings.Contains(stdout, "omit-this-comment") || strings.Contains(stdout, "vendors.opencode") {
		t.Error("vendor table retained comment or vendor prefix")
	}
}

// TestVaultListNestedOverlayRowsPerProfileNoPlans: profiles render one row each under PROVIDER; no KIND, no PLANS.
func TestVaultListNestedOverlayRowsPerProfileNoPlans(t *testing.T) {
	vaulttest.UseFreshKey(t)
	t.Setenv("COLUMNS", "200")
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{
		"databricks":{"dev":{"auth":{"type":"none"},"models":{"opus":"o1"}},"qa":{"auth":{"type":"none"}}},
		"local":{"ollama":null}, "github":{"empty":{}}},
		"agentPlans":{"copilot":[{"tier":"standard","id":"fake-plan"}]}}`)
	code, stdout, _ := hosttest.CaptureOutput(t, func() int { return cmdVault([]string{"ls", "providers", "--format", "text"}) })
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	got := strings.Join(strings.Fields(stdout), " ")
	for _, want := range []string{"NAME PROVIDER AUTH OPUS", "dev databricks none o1", "qa databricks none", "ollama local (removed)"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "PLANS") || strings.Contains(stdout, "KIND") || strings.Contains(stdout, "SONNET") {
		t.Errorf("PLANS, KIND and all-empty columns must be hidden:\n%s", stdout)
	}
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"local":{"ollama":null},"github":{"empty":{}}}}`)
	_, stdout, _ = hosttest.CaptureOutput(t, func() int { return cmdVault([]string{"ls", "--format", "text"}) })
	if strings.Contains(stdout, "PROVIDERS") {
		t.Errorf("a section with only empty/removed rows must not render:\n%s", stdout)
	}
}

// A secret written by vault.Store (what `vault set` uses) shows in `vault ls` masked, with its length.
func TestVaultListShowsStoredSecretMasked(t *testing.T) {
	vaulttest.UseFreshKey(t)
	const val = "abcdefghijklmnopqrstuvwxyz0123"
	if err := vault.Store("ZZ_STORE_SECRET", val); err != nil {
		t.Fatal(err)
	}
	code, out, _ := hosttest.CaptureOutput(t, func() int { return cmdVault([]string{"ls", "secrets", "--format", "json"}) })
	if code != 0 || strings.Contains(out, val) {
		t.Fatalf("vault ls: %d (raw value leaked: %v)", code, strings.Contains(out, val))
	}
	var doc struct {
		Secrets []struct {
			Name, Masked string
			Length       int
		} `json:"secrets"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("vault ls output not JSON: %v\n%s", err, out)
	}
	for _, e := range doc.Secrets {
		if e.Name == "ZZ_STORE_SECRET" {
			if e.Masked == "" || e.Length != len(val) {
				t.Errorf("entry = %+v, want a mask and length %d", e, len(val))
			}
			return
		}
	}
	t.Fatalf("ZZ_STORE_SECRET missing from vault ls: %s", out)
}

// Captured output defaults to JSON; bare and explicit JSON keep Export's sections and add masked secrets.
func TestVaultListJSONMatchesExport(t *testing.T) {
	for _, args := range [][]string{{"ls"}, {"ls", "--format", "json"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			vaulttest.UseFreshKey(t)
			seedVaultListFixture(t)
			want, err := vaultdoc.Export()
			if err != nil {
				t.Fatal(err)
			}
			code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdVault(args) })
			if code != 0 {
				t.Fatalf("vault %q = %d, stderr = %q", args, code, stderr)
			}
			checkVaultListJSON(t, stdout, want)
		})
	}
}

func checkVaultListJSON(t *testing.T, stdout string, exported []byte) {
	t.Helper()
	if strings.Contains(stdout, "tvly-dev-secret-leak-me-not") {
		t.Fatal("JSON leaked a raw secret")
	}
	var got, want map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(exported, &want); err != nil {
		t.Fatal(err)
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(got["secrets"], &rows); err != nil || len(rows) != 1 {
		t.Fatal("expected one masked secret row")
	}
	// "routing" is derived from the providers overlay's nested routing object, not one of Export's
	// own top-level sections; just check it decodes, then drop it before the section-by-section compare.
	if r, ok := got["routing"]; ok {
		var routing providers.Routing
		if err := json.Unmarshal(r, &routing); err != nil {
			t.Fatalf("routing section: %v", err)
		}
		delete(got, "routing")
	}
	for _, row := range rows {
		checkSecretRow(t, row)
	}
	delete(got, "secrets")
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(want)
	if !bytes.Equal(a, b) {
		t.Fatal("list JSON changed Export sections")
	}
}

// checkSecretRow asserts a JSON secret row has exactly name/masked/length matching the fixture's stored metadata.
func checkSecretRow(t *testing.T, row map[string]json.RawMessage) {
	t.Helper()
	if len(row) != 3 || row["name"] == nil || row["masked"] == nil || row["length"] == nil {
		t.Fatal("secret keys must be exactly length, masked, name")
	}
	var name, masked string
	var length int
	_ = json.Unmarshal(row["name"], &name)
	_ = json.Unmarshal(row["masked"], &masked)
	_ = json.Unmarshal(row["length"], &length)
	stored, err := vault.ListStored(nil)
	if err != nil || len(stored) != 1 {
		t.Fatal("could not read fixture metadata")
	}
	if name != "TAVILY_API_KEY" || masked != stored[0].Masked() || length != stored[0].Len {
		t.Fatal("secret metadata differs from stored masked metadata")
	}
}

// Seed every config section and a fake secret that must only appear masked.
func seedVaultListFixture(t *testing.T) {
	t.Helper()
	vaulttest.Put(t, vault.Pins, "HARNESS_PROVIDER", "anthropic")
	vaulttest.Put(t, vault.Env, "KEEP_ENV", "keepvalue")
	vaulttest.Put(t, vault.Overlay, "providers",
		`{"default":"anthropic","fallback":["one","two"],"providers":{"openai":null,"anthropic":{"custom":{"models":{"opus":"m1"}}}}}`)
	vaulttest.Put(t, vault.Overlay, "agents", `{}`)
	vaulttest.Put(t, vault.Overlay, "settings", `{"env":{"FOO":"bar"},"flag":true}`)
	vaulttest.Put(t, vault.Meta, "key_command", `["pass","show","x"]`)
	vaulttest.Secret(t, "TAVILY_API_KEY", "tvly-dev-secret-leak-me-not")
}

// List renders providers as tables and keeps generic config rows without exposing values.
func TestVaultListTextFixtureFlattensEverySection(t *testing.T) {
	vaulttest.UseFreshKey(t)
	seedVaultListFixture(t)
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdVault([]string{"ls", "--format", "text"}) })
	if strings.Contains(stdout+stderr, "tvly-dev-secret-leak-me-not") {
		t.Fatal("vault ls leaked a raw secret value")
	}
	if code != 0 {
		t.Fatalf("vault ls --format text = %d, stderr = %q", code, stderr)
	}
	for _, head := range []string{"PINS", "ENV", "PROVIDERS", "SETTINGS", "VAULT"} {
		if !strings.Contains(stdout, head+"\n") {
			t.Errorf("stdout missing heading %q; stdout = %q", head, stdout)
		}
	}
	for _, tc := range []struct{ key, value string }{
		{"HARNESS_PROVIDER", "anthropic"}, // PINS
		{"KEEP_ENV", "keepvalue"},         // ENV
		{"default", "anthropic"},          // PROVIDERS top-level sibling of providers.*
		{"fallback", "one → two"},
		{"env.FOO", "bar"},                     // SETTINGS nested
		{"flag", "true"},                       // SETTINGS scalar
		{"key_command", `["pass","show","x"]`}, // VAULT
	} {
		if !vaultTextHasRow(stdout, tc.key, tc.value) {
			t.Errorf("stdout missing row %s=%s; stdout = %q", tc.key, tc.value, stdout)
		}
	}
	checkAgentAndProviderTables(t, stdout)
}

// checkAgentAndProviderTables asserts text list output has an AGENT table and the fixture's provider rows.
func checkAgentAndProviderTables(t *testing.T, stdout string) {
	t.Helper()
	if !strings.Contains(stdout, "AGENT\n  claude\n") || strings.Contains(stdout, "AGENTS") {
		t.Errorf("stdout = %q, want an AGENT table with no AGENTS heading", stdout)
	}
	for _, row := range []string{
		"NAME PROVIDER AUTH OPUS",
		"custom anthropic m1", "openai openai (removed)",
	} {
		if !strings.Contains(strings.Join(strings.Fields(stdout), " "), row) {
			t.Errorf("missing provider table row %q", row)
		}
	}
}

// vaultTextHasRow reports whether the rendered table has a row for key with exactly value,
// tolerant of the column-alignment padding (vaultSection pads keys to the widest in the section).
func vaultTextHasRow(stdout, key, value string) bool {
	re := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(key) + `\s\s+` + regexp.QuoteMeta(value) + `$`)
	return re.MatchString(stdout)
}

// A bare list hides empty sections; named sections still render (none), including absent overlay documents.
func TestVaultListTextEmptyVaultAllSectionsNone(t *testing.T) {
	vaulttest.UseFreshKey(t)
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdVault([]string{"ls", "--format", "text"}) })
	if code != 0 || !strings.HasPrefix(stdout, "  AGENT\n  claude\n") {
		t.Fatalf("bare vault ls = %d %q (stderr %q), want 0 and only the AGENT table", code, stdout, stderr)
	}
	named := append([]string{"ls"}, vaultdoc.Sections...)
	code, stdout, stderr = hosttest.CaptureOutput(t, func() int { return cmdVault(append(named[:len(named)-1], "--format", "text")) })
	if code != 0 {
		t.Fatalf("vault ls <sections> = %d, stderr = %q", code, stderr)
	}
	for _, head := range []string{"SECRETS", "PINS", "ENV", "PROVIDERS", "SETTINGS"} {
		if !strings.Contains(stdout, head+"\n  (none)\n") {
			t.Errorf("stdout = %q, want %s rendered as (none)", stdout, head)
		}
	}
	code, stdout, _ = hosttest.CaptureOutput(t, func() int { return cmdVault([]string{"ls", "vault", "--format", "text"}) })
	if code != 0 || stdout != "VAULT\n  (none)\n" {
		t.Errorf("vault ls vault = %d %q, want VAULT rendered as (none) when key_command is unset", code, stdout)
	}
}

// TestVaultListTextTruncatesToWidth: a row longer than $COLUMNS is clipped to that many runes and
// ends in the ellipsis, rather than wrapping or overflowing the terminal.
func TestVaultListTextTruncatesToWidth(t *testing.T) {
	vaulttest.UseFreshKey(t)
	vaulttest.Put(t, vault.Env, "LONG_ENV_NAME_FOR_WIDTH_TEST", strings.Repeat("x", 200))
	t.Setenv("COLUMNS", "40")
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdVault([]string{"ls", "--format", "text"}) })
	if code != 0 {
		t.Fatalf("vault ls --format text = %d, stderr = %q", code, stderr)
	}
	for line := range strings.SplitSeq(stdout, "\n") {
		if n := utf8.RuneCountInString(line); n > 40 {
			t.Errorf("line %q is %d runes, want <= 40 (COLUMNS)", line, n)
		}
	}
	found := false
	for line := range strings.SplitSeq(stdout, "\n") {
		if strings.Contains(line, "LONG_ENV_NAM…_WIDTH_TEST") && strings.HasSuffix(line, "…") {
			found = true
		}
	}
	if !found {
		t.Errorf("stdout = %q, want the over-width row clipped with an ellipsis", stdout)
	}
}

func TestVaultSetTailscaleRollsBackSecondWrite(t *testing.T) {
	for _, prior := range []string{"", "original-client-id"} {
		t.Run(map[bool]string{false: "new", true: "replace"}[prior != ""], func(t *testing.T) {
			vaulttest.UseFreshKey(t)
			vaulttest.Secret(t, "TAILSCALE_API_KEY", "unrelated-api-key")
			if prior != "" {
				vaulttest.Secret(t, "TAILSCALE_OAUTH_CLIENT_ID", prior)
			}
			blockSecretWrite(t, "TAILSCALE_OAUTH_CLIENT_SECRET")
			vaultTestStdin(t, "replacement-id\nreplacement-secret\n")
			code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
				return cmdVault([]string{"set", "--tailscale"})
			})
			if code != 1 || !strings.Contains(stderr, bolterrors.ErrIncompatibleValue.Error()) {
				t.Fatalf("second credential write: exit %d, stdout=%q, stderr=%q", code, stdout, stderr)
			}
			assertVaultSecretUnchanged(t, "TAILSCALE_OAUTH_CLIENT_ID", prior)
			assertVaultSecretUnchanged(t, "TAILSCALE_API_KEY", "unrelated-api-key")
		})
	}
}

func assertVaultSecretUnchanged(t *testing.T, name, want string) {
	t.Helper()
	got, found, err := vault.ReadSecret(name)
	if err != nil || found != (want != "") || got != want {
		t.Errorf("failed OAuth write changed %s: found=%v, err=%v", name, found, err)
	}
}

// A nested bucket is readable as absent but makes bbolt reject a credential write at that exact key.
func blockSecretWrite(t *testing.T, name string) {
	t.Helper()
	db, err := bbolt.Open(vault.Path(), 0600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.Bucket([]byte(vault.Secrets)).CreateBucket([]byte(name))
		return err
	})
	closeErr := db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
}

func TestVaultImportFileYesSkipsTerminalConfirm(t *testing.T) {
	vaulttest.UseFreshKey(t)
	path := filepath.Join(t.TempDir(), "import.txt")
	if err := os.WriteFile(path, []byte("REGRESSION_IMPORT=synthetic-import-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	master, slave := proctest.PTY(t)
	old := os.Stdin
	os.Stdin = slave
	t.Cleanup(func() { os.Stdin = old })
	if !tui.StdinIsTerminal() {
		t.Fatal("import regression needs terminal stdin")
	}
	if _, err := master.WriteString("n\n"); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
		return cmdVault([]string{"import", path, "--yes"})
	})
	if code != 0 || strings.Contains(stdout+stderr, "Store 1 secret") {
		t.Fatalf("import FILE --yes prompted or failed: exit %d, stdout=%q, stderr=%q", code, stdout, stderr)
	}
	got, found, err := vault.ReadSecret("REGRESSION_IMPORT")
	if err != nil || !found || got != "synthetic-import-value" {
		t.Fatal("import FILE --yes did not store the secret")
	}
}

func TestVaultGetFailsOnUnwritableStdout(t *testing.T) {
	for _, export := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "export"}[export], func(t *testing.T) {
			vaulttest.UseFreshKey(t)
			vaulttest.Secret(t, "REGRESSION_GET", "synthetic-get-value")
			args := []string{"get", "REGRESSION_GET"}
			if export {
				args = append(args, "--export")
			}
			closed, err := os.CreateTemp(t.TempDir(), "closed-stdout")
			if err != nil || closed.Close() != nil {
				t.Fatal(err)
			}
			code, _, stderr := hosttest.CaptureOutput(t, func() int {
				old := os.Stdout
				os.Stdout = closed
				defer func() { os.Stdout = old }()
				return cmdVault(args)
			})
			if code != 1 || !strings.Contains(stderr, "vault get: write ") || !strings.Contains(stderr, os.ErrClosed.Error()) {
				t.Fatalf("get with unwritable stdout: exit %d, stderr=%q", code, stderr)
			}
		})
	}
}

func TestVaultGetKeyRefusesTTY(t *testing.T) {
	vaulttest.UseFreshKey(t)
	_, slave := proctest.PTY(t)
	assertGetKeyRefusesTTY(t, slave)
}
