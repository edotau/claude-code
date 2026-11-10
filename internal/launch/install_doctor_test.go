package launch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

type credentialProbeTest struct {
	inflight, maxSeen atomic.Int32
	gate, over        chan struct{}
	once, overOnce    sync.Once
	byPtr             map[*providers.Provider]string
}

func (s *credentialProbeTest) probe(_ context.Context, p *providers.Provider) (string, string) {
	cur := s.inflight.Add(1)
	defer s.inflight.Add(-1)
	for {
		m := s.maxSeen.Load()
		if cur <= m || s.maxSeen.CompareAndSwap(m, cur) {
			break
		}
	}
	if cur >= credentialProbeWorkers {
		s.once.Do(func() { close(s.gate) })
	}
	if cur > credentialProbeWorkers {
		s.overOnce.Do(func() { close(s.over) })
	}
	select {
	case <-s.gate:
	case <-time.After(5 * time.Second): // only an implementation below the limit reaches this
	}
	select {
	case <-s.over:
	case <-time.After(100 * time.Millisecond): // gives a probe beyond the limit time to show up
	}
	n := s.byPtr[p]
	return "probe-" + n, "detail-" + n
}

func TestProviderCredentialChecksRunConcurrentlyInOrder(t *testing.T) {
	hermetic(t, "")
	names := []string{"p-0", "p-1", "p-2", "p-3", "p-4", "p-5", "p-6", "p-7"}
	reg := &providers.Registry{Default: "elsewhere", Providers: map[string]*providers.Provider{}}
	state := &credentialProbeTest{gate: make(chan struct{}), over: make(chan struct{}), byPtr: map[*providers.Provider]string{}}
	for _, n := range names {
		p := &providers.Provider{Auth: providers.Auth{Type: providers.AuthNone}}
		reg.Providers[n] = p
		state.byPtr[p] = n
	}
	old := probeCredential
	t.Cleanup(func() { probeCredential = old })
	probeCredential = state.probe
	rows := providerCredentialChecks(reg, providers.Selection{}, errors.New("no session"), names)
	assertCredentialProbeRows(t, rows, names)
	if m := state.maxSeen.Load(); m < 2 {
		t.Errorf("max in-flight probes = %d, want >= 2 (probes must overlap)", m)
	} else if m > credentialProbeWorkers {
		t.Errorf("max in-flight probes = %d, want <= %d (worker limit)", m, credentialProbeWorkers)
	}
}

func assertCredentialProbeRows(t *testing.T, rows []Check, names []string) {
	t.Helper()
	if len(rows) != len(names) {
		t.Fatalf("got %d rows, want %d", len(rows), len(names))
	}
	for i, n := range names {
		if rows[i].Name != "credential "+n {
			t.Errorf("row %d = %q, want %q", i, rows[i].Name, "credential "+n)
		}
		if want := "probe-" + n + ": detail-" + n; !rows[i].OK || !strings.Contains(rows[i].Detail, want) {
			t.Errorf("row %s = %+v, want OK with detail containing %q", n, rows[i], want)
		}
	}
}

func TestToolPATHCopiesAndExtras(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", scratch)
	bin := paths.BinDir()
	primary := pathstest.FakeVendor(t, "opencode", "echo 1.0.0")
	aBin := vendorPATHFixture(t, "opencode", "echo 1.0.0")
	bBin := vendorPATHFixture(t, "opencode", "echo 2.0.0")
	a, b, c := filepath.Dir(aBin), filepath.Dir(bBin), filepath.Join(scratch, "C")
	// C/opencode is a symlink to A/opencode: same real file, must dedupe.
	if err := os.MkdirAll(c, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(aBin, filepath.Join(c, "opencode")); err != nil {
		t.Fatal(err)
	}
	join := func(dirs ...string) string { return strings.Join(dirs, string(os.PathListSeparator)) }
	cases := []struct {
		name, path string
		wantCopies []string
	}{
		{"bin first", join(bin, a, b), []string{aBin, bBin}},
		{"tool before bin", join(a, bin, b), []string{aBin, bBin}},
		{"no bin dir", join(a, b), []string{aBin, bBin}},
		{"symlink dedup", join(bin, a, c), []string{aBin}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", tc.path)
			copies := toolPATHCopies("opencode")
			if !slices.Equal(copies, tc.wantCopies) {
				t.Fatalf("toolPATHCopies = %v; want %v", copies, tc.wantCopies)
			}
		})
	}
	status := tools.Status{Name: "opencode", Path: aBin, Version: "1.0.0"}
	for _, order := range []string{join(a, bin, b), join(bin, a, b)} {
		t.Run(order, func(t *testing.T) {
			t.Setenv("PATH", order)
			detail := toolDoctorDetail(status)
			want := primary + " 1.0.0 (direct install in bin/)" + " · also on PATH: " + aBin + " 1.0.0 · also on PATH: " + bBin + " 2.0.0"
			if detail != want {
				t.Errorf("detail = %q, want %q", detail, want)
			}
		})
	}
}

func doctorVendorLink(t *testing.T, kind, source, dst string) string {
	t.Helper()
	if err := os.Remove(dst); err != nil {
		t.Fatal(err)
	}
	target := source
	want := ""
	switch kind {
	case "link":
		want = dst + " → " + source + " 1.2.3"
	case "broken link":
		target = filepath.Join(t.TempDir(), "missing")
		want = "opencode: " + dst + " is a broken link (run claude-code install) · on PATH: " + source
	case "retired shim":
		target = filepath.Join(t.TempDir(), "claude-code")
		if err := os.WriteFile(target, []byte("must not execute"), 0o755); err != nil {
			t.Fatal(err)
		}
		want = "opencode: " + dst + " is the retired claude-code shim (run claude-code install) · on PATH: " + source
	}
	if err := os.Symlink(target, dst); err != nil {
		t.Fatal(err)
	}
	return want
}

func TestDoctorVendorRows(t *testing.T) {
	for _, kind := range []string{"link", "direct", "broken link", "retired shim"} {
		t.Run(kind, func(t *testing.T) {
			doctorScratch(t)
			t.Setenv("HOME", t.TempDir())
			source := vendorPATHFixture(t, "opencode", "echo 1.2.3")
			t.Setenv("PATH", filepath.Dir(source))
			dst := pathstest.FakeVendor(t, "opencode", "echo 4.5.6")
			want := dst + " 4.5.6 (direct install in bin/)" + " · also on PATH: " + source + " 1.2.3"
			if kind != "direct" {
				want = doctorVendorLink(t, kind, source, dst)
			}
			checks, _ := DoctorChecks()
			row := findCheck(checks, "tool opencode")
			if row == nil {
				t.Fatal("missing tool opencode row")
			}
			if row.Detail != want {
				t.Errorf("detail = %q, want %q", row.Detail, want)
			}
			if wantOK := kind == "link" || kind == "direct"; row.OK != wantOK {
				t.Errorf("OK = %v, want %v", row.OK, wantOK)
			}
		})
	}
}

func TestDoctorAgentVersionRows(t *testing.T) {
	hermetic(t, "")
	pathstest.FakeVendor(t, "copilot", "echo 'GitHub Copilot CLI 1.0.100.'")
	checks, _ := DoctorChecks()
	c := findCheck(checks, "agent version copilot")
	if c == nil {
		t.Fatal("no `agent version copilot` row")
	}
	if !strings.HasPrefix(c.Detail, "warn 1.0.100 below 1.0.104") || c.Hard {
		t.Errorf("row = %+v, want detail `warn 1.0.100 below 1.0.104...` and Hard=false", *c)
	}
	hermetic(t, "")
	t.Setenv("PATH", t.TempDir())
	checks, _ = DoctorChecks()
	c = findCheck(checks, "agent version copilot")
	if c == nil || c.Detail != "not installed" || c.Hard {
		t.Errorf("missing binary row = %+v, want `not installed`, non-hard", c)
	}
}

func TestToolPATHVersionProbesUnchangedBinaryOnce(t *testing.T) {
	hermetic(t, "")
	dir := t.TempDir()
	counter := filepath.Join(dir, "count")
	bin := filepath.Join(dir, "fake")
	script := "#!/bin/sh\necho x >> '" + counter + "'\necho 'fake 7.8.9'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if got := toolPATHVersion(bin); got != "fake 7.8.9" {
			t.Fatalf("toolPATHVersion = %q", got)
		}
	}
	data, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "\n"); n != 1 {
		t.Fatalf("two lookups spawned the binary %d times, want 1", n)
	}
}

func TestStrayClaudeJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if ok, _ := strayClaudeJSON(); !ok {
		t.Fatal("no stub must pass")
	}
	stub := filepath.Join(home, ".claude", ".claude.json")
	if err := os.MkdirAll(filepath.Dir(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stub, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, detail := strayClaudeJSON(); ok || !strings.Contains(detail, stub) {
		t.Errorf("stub present: ok=%v detail=%q", ok, detail)
	}
}

func TestVaultDoctorChecks(t *testing.T) {
	t.Run("missing vault", testVaultDoctorMissing)
	t.Run("secrets and retired", testVaultDoctorSecrets)
	t.Run("corrupt retired metadata", testVaultDoctorCorrupt)
	t.Run("one vault row, not two", testVaultDoctorSingleRow)
}

func testVaultDoctorMissing(t *testing.T) {
	t.Helper()
	hermetic(t, "")
	checks := vaultDoctorChecks()
	c := findCheck(checks, "vault")
	if c == nil || c.Detail != "not created yet" || !c.OK {
		t.Errorf("vault row = %+v", c)
	}
	for _, name := range []string{"vault perms", "keyring tool"} {
		if c := findCheck(checks, name); c == nil || !c.OK {
			t.Errorf("missing healthy %s row", name)
		}
	}
}

func testVaultDoctorSecrets(t *testing.T) {
	t.Helper()
	hermetic(t, "")
	t.Setenv("HARNESS_VAULT_KEY", "")
	vaulttest.Keyring(t)
	vaulttest.Secret(t, "FOO_API_KEY", "sk-test-0123456789")
	if _, _, err := vault.RotateKey(); err != nil {
		t.Fatal(err)
	}
	c := findCheck(vaultDoctorChecks(), "vault")
	if c == nil || !c.OK {
		t.Fatalf("vault row = %+v", c)
	}
	for _, want := range []string{vault.Path(), "key keyring", "1 secret(s)", "1 retired"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail %q lacks %q", c.Detail, want)
		}
	}
	if strings.Contains(c.Detail, "sk-test") {
		t.Error("doctor printed a secret")
	}
}

func testVaultDoctorCorrupt(t *testing.T) {
	t.Helper()
	hermetic(t, "")
	vaulttest.Put(t, vault.Meta, "retired", `["old", 42]`)
	checks := vaultDoctorChecks()
	for _, name := range []string{"vault", "vault retired keys"} {
		c := findCheck(checks, name)
		if c == nil || c.OK || !strings.Contains(c.Detail, "retired key IDs are corrupt") {
			t.Errorf("%s row = %+v, want corruption diagnostic", name, c)
		}
	}
}

func testVaultDoctorSingleRow(t *testing.T) {
	t.Helper()
	hermetic(t, "")
	t.Setenv("HARNESS_VAULT_KEY", "")
	vaulttest.Keyring(t)
	vaulttest.Secret(t, "FOO_API_KEY", "sk-test-0123456789")
	n := 0
	for _, c := range vaultDoctorChecks() {
		if c.Name == "vault" || c.Name == "vault master key" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("vaultDoctorChecks has %d vault/master-key row(s), want exactly 1 (the \"vault\" row carries the master-key status)", n)
	}
}
