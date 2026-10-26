package databricks

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
)

const dbxCfgFixture = `[DEFAULT]
host = https://leak.example.cloud.databricks.com

[__settings__]
x = y

[alice-u2m]
host = https://alice.example.cloud.databricks.com
auth_type = databricks-cli

[svc-m2m]
host = https://svc.example.cloud.databricks.com
client_id = cid-1
client_secret = secret-value-1

[pat-one]
host = https://pat.example.cloud.databricks.com
token = dapi-token-value
`

func TestVaultImportDatabrickscfg(t *testing.T) {
	useVaultTest(t)
	cfg := filepath.Join(t.TempDir(), "cfg")
	if err := os.WriteFile(cfg, []byte(dbxCfgFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	base := []string{"--file", cfg}

	code, out, _ := captureOutput(t, func() int { return importCfg(base) })
	if code != 0 || !strings.Contains(out, "dry-run") || strings.Contains(out, "secret-value-1") {
		t.Fatalf("dry-run: %d %q", code, out)
	}
	if reg, _ := providers.Load(); reg != nil && reg.Providers["svc-m2m"] != nil {
		t.Fatal("dry-run wrote a row")
	}

	args := append(slices.Clone(base), "--apply", "--seal-secrets", "--rename", "pat-one=pat-renamed")
	code, out, _ = captureOutput(t, func() int { return importCfg(args) })
	if code != 0 || strings.Contains(out, "secret-value-1") || strings.Contains(out, "dapi-token-value") {
		t.Fatalf("apply: %d %q", code, out)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"alice-u2m", "svc-m2m", "pat-renamed"} {
		if reg.Providers[id] == nil {
			t.Errorf("row %s missing", id)
		}
	}
	if reg.Providers["DEFAULT"] != nil || reg.Providers["__settings__"] != nil {
		t.Error("DEFAULT/__settings__ imported")
	}
	names := secrets.Names()
	for _, n := range []string{"PROVIDER_SVC_M2M_CLIENT_SECRET", "PROVIDER_PAT_RENAMED_PAT"} {
		if !slices.Contains(names, n) {
			t.Errorf("secret %s not sealed (have %v)", n, names)
		}
	}

	code, out, _ = captureOutput(t, func() int { return importCfg(append(slices.Clone(base), "--apply")) })
	if code != 0 || !strings.Contains(out, "row exists") {
		t.Errorf("re-import should skip existing: %d %q", code, out)
	}
}

// writeCfg writes a databrickscfg fixture and returns its path.
func writeCfg(t *testing.T, body string) string {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "cfg")
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestImportCfgRefusals(t *testing.T) {
	const one = "[solo]\nhost = https://solo.example.cloud.databricks.com\nauth_type = databricks-cli\n"
	cases := []struct {
		name   string
		cfg    string
		args   []string
		want   string
		refuse bool // true: non-zero exit; false: a skipped row (exit 0, the note says why)
	}{
		{"unknown profile", one, []string{"--apply", "--profile", "ghost"}, `no profile "ghost" in the config`, true},
		{"empty config", "", []string{"--apply"}, "no profiles in", true},
		{"bad rename", one, []string{"--apply", "--rename", "solo"}, "want old=new", true},
		{"default only", "[DEFAULT]\nhost = https://d.example.cloud.databricks.com\n", []string{"--apply"},
			"never imported", false},
		{"bad id", one, []string{"--apply", "--rename", "solo=bad id!"}, "id must match", false},
		{"http host", "[web]\nhost = http://web.example.cloud.databricks.com\nauth_type = databricks-cli\n",
			[]string{"--apply"}, "no usable https host", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			useVaultTest(t)
			args := append([]string{"--file", writeCfg(t, c.cfg)}, c.args...)
			code, out, errOut := captureOutput(t, func() int { return importCfg(args) })
			if all := out + errOut; !strings.Contains(all, c.want) {
				t.Errorf("output %q lacks %q (code %d)", all, c.want, code)
			}
			if c.refuse == (code == 0) {
				t.Errorf("exit %d, refuse=%v", code, c.refuse)
			}
			reg, _ := providers.Load()
			for _, id := range []string{"solo", "web", "DEFAULT", "bad id!", "ghost"} {
				if reg != nil && reg.Providers[id] != nil {
					t.Errorf("row %q was written", id)
				}
			}
		})
	}
}
