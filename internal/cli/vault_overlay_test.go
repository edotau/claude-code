package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaultdoc"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// vault overlay unset: off a terminal it needs --yes, --dry-run writes nothing, an unknown key is refused.
func TestVaultOverlayUnset(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "proxy-teams", `{"schema":1}`)
	for _, c := range []struct {
		args []string
		code int
		out  string
		left bool
	}{
		{[]string{"unset", "providers", "--yes"}, 1, "not a removable overlay key", true},
		{[]string{"unset", "proxy-teams"}, 2, "pass --yes", true},
		{[]string{"unset", "--dry-run", "proxy-teams"}, 0, "would be removed", true},
		{[]string{"ls"}, 0, "proxy-teams  12 bytes", true},
		{[]string{"unset", "--yes", "proxy-teams"}, 0, "overlay proxy-teams removed (was 12 bytes)", false},
		{[]string{"unset", "--yes", "proxy-teams"}, 0, "not set; nothing written", false},
	} {
		code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdVaultOverlay(c.args) })
		var left bool
		_ = vault.View(func(tx *vault.Tx) error { _, left = tx.Get(vault.Overlay, "proxy-teams"); return nil })
		if code != c.code || left != c.left || !strings.Contains(stdout+stderr, c.out) {
			t.Errorf("vault overlay %v: exit %d left %v output %q; want exit %d left %v containing %q",
				c.args, code, left, stdout+stderr, c.code, c.left, c.out)
		}
	}
}

// vault overlay set: an over-cap proxy-teams document is refused and nothing is written.
func TestVaultOverlaySetRefusesOverCap(t *testing.T) {
	vaulttest.Use(t)
	doc := filepath.Join(t.TempDir(), "teams.json")
	bad := `{"schema":1,"teams":[{"name":"phase","kinds":["group"],"host":"phase","lifetime":"phase",` +
		`"reserved":1024,"borrow":"never","fallback":["shared"]}]}`
	if err := os.WriteFile(doc, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return cmdVaultOverlay([]string{"set", "proxy-teams", "--file", doc}) })
	var stored bool
	_ = vault.View(func(tx *vault.Tx) error { _, stored = tx.Get(vault.Overlay, "proxy-teams"); return nil })
	if code == 0 || stored || !strings.Contains(stderr, "nothing written") {
		t.Errorf("exit %d stored %v stderr %q; want refusal with nothing stored", code, stored, stderr)
	}
}

func TestVaultOverlaySetRefusesMalformed(t *testing.T) {
	for _, c := range []struct{ key, bad string }{
		{"providers", `{"providers":{"openai":{"scratch":{"protocol":"openai","base_url":"https://example.invalid","auth":{"type":"private-value"}}}}}`},
		{"agent-records", `{"private-value":{"model":[]}}`},
		{"opencode-models", `[{"id":"private-value","responses":"not-a-bool"}]`},
		{"github-auth", "work\nprivate-value"},
	} {
		t.Run(c.key, func(t *testing.T) {
			vaulttest.Use(t)
			doc := filepath.Join(t.TempDir(), "overlay")
			if err := os.WriteFile(doc, []byte(c.bad), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, prior := range []string{"", "previous-value"} {
				vaulttest.Put(t, vault.Overlay, c.key, prior)
				code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
					return cmdVaultOverlay([]string{"set", c.key, "--file", doc})
				})
				if code != 1 || stdout != "" || !strings.Contains(stderr, "nothing written") || strings.Contains(stderr, "private-value") {
					t.Fatalf("exit=%d stdout=%q stderr=%q; want a value-free refusal", code, stdout, stderr)
				}
				assertOverlayStored(t, c.key, prior)
			}
			events, err := audit.Read(time.Time{})
			if err != nil || len(events) != 0 {
				t.Fatalf("refusal audit = %v, %v; want no events", events, err)
			}
		})
	}
}

// agent-records: overlay set gives a bad record the configure path's message, and a valid record passes both.
func TestVaultOverlaySetAgentRecordsShareValidator(t *testing.T) {
	vaulttest.Use(t)
	want := configure.ValidateAgentField("claude", "EFFORT", "turbo")
	if want == nil || configure.ValidateAgentField("claude", "EFFORT", "high") != nil {
		t.Fatalf("configure path: turbo = %v, want an error; high must pass", want)
	}
	for doc, wantErr := range map[string]bool{`{"claude":{"effort":"turbo"}}`: true, `{"claude":{"effort":"high"}}`: false} {
		f := filepath.Join(t.TempDir(), "records")
		if err := os.WriteFile(f, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		code, _, stderr := hosttest.CaptureOutput(t, func() int { return cmdVaultOverlay([]string{"set", "agent-records", "--file", f}) })
		if refused := code != 0; refused != wantErr || wantErr && !strings.Contains(stderr, want.Error()) {
			t.Errorf("%s: exit %d stderr %q; want refused=%v naming %q", doc, code, stderr, wantErr, want)
		}
	}
}

func TestVaultOverlaySetStoresAndAudits(t *testing.T) {
	for _, c := range []struct{ key, doc string }{
		{"proxy-teams", `{"schema":1}`},
		{"providers", `{"providers":{"openai":{"scratch":{"protocol":"openai","base_url":"https://example.invalid","auth":{"type":"none"},"headers":{"X-Private":"private-value"},"models":{"opus":"model-x"}}}}}`},
		{"agent-records", `{"claude":{"route":{"provider":"openai","profile":"scratch"},"model":"model-x","launch":{"env":["MODE=private-value"]}}}`},
		{"opencode-models", `[{"id":"model-x","label":"private-value","responses":true}]`},
		{"github-auth", "private-value\n"},
	} {
		t.Run(c.key, func(t *testing.T) {
			vaulttest.Use(t)
			doc := filepath.Join(t.TempDir(), "overlay")
			if err := os.WriteFile(doc, []byte(c.doc), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, prior := range []string{"", c.doc} {
				code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
					return cmdVaultOverlay([]string{"set", c.key, "--file", doc})
				})
				want := fmt.Sprintf("overlay %s set: %d -> %d bytes\n", c.key, len(prior), len(c.doc))
				if code != 0 || stdout != "" || stderr != want {
					t.Fatalf("exit=%d stdout=%q stderr=%q; want %q", code, stdout, stderr, want)
				}
				assertOverlayStored(t, c.key, c.doc)
			}
			assertOverlaySetAudit(t, c.key, len(c.doc))
		})
	}
}

func TestVaultOverlaySetStdin(t *testing.T) {
	vaulttest.Use(t)
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	const doc = `{"claude":{"model":"model-x"}}`
	if _, err := f.WriteString(doc); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = old })
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		return cmdVaultOverlay([]string{"set", "agent-records", "--file", "-"})
	})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	assertOverlayStored(t, "agent-records", doc)
}

func TestVaultOverlaySetBackupAndUnknownRefused(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers.prev", "previous-value")
	for _, key := range []string{"providers.prev", "private-value"} {
		code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
			return cmdVaultOverlay([]string{"set", key, "--file", "does-not-exist"})
		})
		if code != 1 || stdout != "" || !strings.Contains(stderr, "no validator") || strings.Contains(stderr, "private-value") {
			t.Errorf("key refusal: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
	}
	assertOverlayStored(t, "providers.prev", "previous-value")
	assertOverlayStored(t, "private-value", "")
	if _, err := vaultdoc.SetOverlay("private-value", "{}"); err == nil || strings.Contains(err.Error(), "private-value") {
		t.Fatalf("direct setter must refuse without echoing the key: %v", err)
	}
}

func TestVaultOverlaySetProvidersWarningsDoNotLeak(t *testing.T) {
	vaulttest.Use(t)
	doc := filepath.Join(t.TempDir(), "providers.json")
	const raw = `{"providers":{"github":{"private-value":{"protocol":"openai","base_url":"https://example.invalid","auth":{"type":"none"},"login":"ultra"}}}}`
	if err := os.WriteFile(doc, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
		return cmdVaultOverlay([]string{"set", "providers", "--file", doc})
	})
	if code != 0 || stdout != "" || stderr != fmt.Sprintf("overlay providers set: 0 -> %d bytes\n", len(raw)) {
		t.Fatalf("provider collision warning leaked: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestVaultOverlayHelpWidth(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"set", "--help"}} {
		code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdVaultOverlay(args) })
		if code != 0 {
			t.Fatalf("help %v: exit=%d stderr=%q", args, code, stderr)
		}
		output := stdout + stderr
		for _, key := range vaultdoc.SettableOverlayKeys {
			if !strings.Contains(output, key) {
				t.Errorf("help %v omits %s", args, key)
			}
		}
		for line := range strings.SplitSeq(output, "\n") {
			if utf8.RuneCountInString(line) > 80 {
				t.Errorf("help %v exceeds 80 columns: %q", args, line)
			}
		}
	}
}

func TestVaultOverlaySetProvidersKeepsBackupAndAuthGuard(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers.prev", "retained-backup")
	doc := filepath.Join(t.TempDir(), "providers.json")
	const first = `{"providers":{"openai":{"scratch":{"protocol":"openai","base_url":"https://first.example.invalid","auth":{"type":"none"}}}}}`
	second := strings.ReplaceAll(first, "first.example", "second.example")
	for _, raw := range []string{first, second} {
		if err := os.WriteFile(doc, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		code, _, stderr := hosttest.CaptureOutput(t, func() int {
			return cmdVaultOverlay([]string{"set", "providers", "--file", doc})
		})
		if code != 0 {
			t.Fatalf("exit=%d stderr=%q", code, stderr)
		}
		if raw == first {
			assertOverlayStored(t, "providers.prev", "retained-backup")
		}
	}
	assertOverlayStored(t, "providers.prev", first)
	if err := os.WriteFile(doc, []byte(strings.ReplaceAll(second, `"none"`, `"pat"`)), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
		return cmdVaultOverlay([]string{"set", "providers", "--file", doc})
	})
	if code != 1 || stdout != "" || !strings.Contains(stderr, "provider overlay change refused; nothing written") {
		t.Fatalf("auth change: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	assertOverlayStored(t, "providers", second)
	assertOverlayStored(t, "providers.prev", first)
}

func assertOverlaySetAudit(t *testing.T, key string, size int) {
	t.Helper()
	events, err := audit.Read(time.Time{})
	if err != nil || len(events) != 2 {
		t.Fatalf("audit: %v, %v; want two overlay_set events", events, err)
	}
	for i, event := range events {
		old := 0
		if i == 1 {
			old = size
		}
		if event.Action != audit.OverlaySet || event.Resource != key || event.Detail != fmt.Sprintf("%d -> %d bytes", old, size) {
			t.Errorf("unexpected audit event: %+v", event)
		}
	}
}

func assertOverlayStored(t *testing.T, key, want string) {
	t.Helper()
	if err := vault.View(func(tx *vault.Tx) error {
		got, present := tx.Get(vault.Overlay, key)
		if got != want || present != (want != "") {
			t.Errorf("overlay %s changed unexpectedly (present=%v)", key, present)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
