package clipboard

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withProcVersion points procVersionPath at a fixture file containing content, restoring on cleanup.
func withProcVersion(t *testing.T, content string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "version")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	old := procVersionPath
	procVersionPath = path
	oldOS := doctorGOOS
	doctorGOOS = "linux" // the /proc/version fixture models a Linux host, whatever runs the test
	t.Cleanup(func() { procVersionPath, doctorGOOS = old, oldOS })
}

func TestOnWSLKernel(t *testing.T) {
	withProcVersion(t, "Linux version 5.15.90.1-microsoft-standard-WSL2 (...)\n")
	if !OnWSLKernel() {
		t.Error("microsoft kernel string: want WSL detected")
	}
	withProcVersion(t, "Linux version 6.8.0-generic (buildd@host) ...\n")
	if OnWSLKernel() {
		t.Error("plain Linux kernel string: want WSL NOT detected")
	}
}

func TestOnWSLKernelMissingFile(t *testing.T) {
	old := procVersionPath
	procVersionPath = filepath.Join(t.TempDir(), "does-not-exist")
	t.Cleanup(func() { procVersionPath = old })
	if OnWSLKernel() {
		t.Error("unreadable /proc/version: want WSL NOT detected")
	}
}

// setHome points UserHomeDir (via $HOME) at a scratch dir so ProxyPath never touches the real home.
func setHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func TestInstallProxyAtAbsent(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "sub", "wl-paste")
	var out bytes.Buffer
	if err := installProxyAt(&out, dest, false); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("proxy not written: %v", err)
	}
	if !bytes.Equal(b, ProxyScript) {
		t.Error("written proxy does not match embedded script")
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755", info.Mode().Perm())
	}
	if !strings.Contains(out.String(), "wrote") {
		t.Errorf("output = %q, want a wrote line", out.String())
	}
}

func TestInstallProxyAtCurrent(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "wl-paste")
	if err := os.WriteFile(dest, ProxyScript, 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := installProxyAt(&out, dest, false); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(dest)
	if !bytes.Equal(b, ProxyScript) {
		t.Error("current proxy was rewritten")
	}
	if !strings.Contains(out.String(), "current") {
		t.Errorf("output = %q, want a current line", out.String())
	}
}

func TestInstallProxyAtStaleIsUpdated(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "wl-paste")
	stale := []byte("#!/usr/bin/env bash\n# " + proxyMarker + " (v0, old copy)\necho old\n")
	if err := os.WriteFile(dest, stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := proxyState(dest); got != ProxyStale {
		t.Fatalf("fixture precondition: proxyState = %v, want ProxyStale", got)
	}
	var out bytes.Buffer
	if err := installProxyAt(&out, dest, false); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(dest)
	if !bytes.Equal(b, ProxyScript) {
		t.Error("stale proxy was not updated to the current embedded script")
	}
	if !strings.Contains(out.String(), "wrote") {
		t.Errorf("output = %q, want a wrote line", out.String())
	}
}

func TestInstallProxyAtForeignUntouched(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "wl-paste")
	foreign := []byte("#!/bin/sh\nexec /usr/bin/wl-paste.real \"$@\"\n")
	if err := os.WriteFile(dest, foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	// Teeth: grep the fixture file for OUR marker before asserting non-overwrite — proves the
	// precondition (a genuinely foreign file, not an accidental copy of the proxy) actually holds.
	if bytes.Contains(before, []byte(proxyMarker)) {
		t.Fatal("fixture precondition: foreign file unexpectedly contains the proxy marker")
	}
	var out bytes.Buffer
	if err := installProxyAt(&out, dest, false); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("foreign file was modified: before %q, after %q", before, after)
	}
	if !strings.Contains(out.String(), "skip") || !strings.Contains(out.String(), "not the claude-code proxy") {
		t.Errorf("output = %q, want a skip line naming the foreign file", out.String())
	}
}

func TestInstallProxyAtDryRunDoesNotWrite(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "wl-paste")
	var out bytes.Buffer
	if err := installProxyAt(&out, dest, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Fatal("--dry-run wrote the proxy file")
	}
	if !strings.Contains(out.String(), "would write") {
		t.Errorf("output = %q, want a would-write line", out.String())
	}
}

func TestInstallProxyWSLGate(t *testing.T) {
	home := setHome(t)
	dest := filepath.Join(home, ".local", "bin", "wl-paste")

	withProcVersion(t, "Linux version 6.8.0-generic (buildd@host) ...\n")
	var offWSL bytes.Buffer
	if err := InstallProxy(&offWSL, false); err != nil {
		t.Fatal(err)
	}
	if offWSL.String() != "" {
		t.Errorf("off-WSL output = %q, want silent no-op", offWSL.String())
	}
	if _, err := os.Stat(dest); err == nil {
		t.Fatal("off-WSL InstallProxy wrote the proxy file")
	}

	withProcVersion(t, "Linux version 5.15.90.1-microsoft-standard-WSL2 (...)\n")
	var onWSL bytes.Buffer
	if err := InstallProxy(&onWSL, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(onWSL.String(), "wrote") {
		t.Errorf("WSL output = %q, want a wrote line", onWSL.String())
	}
	b, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("WSL InstallProxy did not write the proxy: %v", err)
	}
	if !bytes.Equal(b, ProxyScript) {
		t.Error("written proxy does not match embedded script")
	}

	// Re-run: now current, reported as such, left byte-identical.
	var again bytes.Buffer
	if err := InstallProxy(&again, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(again.String(), "current") {
		t.Errorf("re-run output = %q, want a current line", again.String())
	}
}

func TestDoctorDetailOffWSL(t *testing.T) {
	setHome(t)
	withProcVersion(t, "Linux version 6.8.0-generic (buildd@host) ...\n")
	ok, detail := DoctorDetail()
	if !ok {
		t.Errorf("off-WSL: ok = false, detail %q", detail)
	}
	if !strings.Contains(detail, "not WSL") {
		t.Errorf("detail = %q, want it to say not WSL", detail)
	}
}

func TestDoctorDetailMissingOnWSL(t *testing.T) {
	setHome(t)
	withProcVersion(t, "Linux version 5.15.90.1-microsoft-standard-WSL2 (...)\n")
	ok, detail := DoctorDetail()
	if ok {
		t.Errorf("missing proxy on WSL: ok = true, detail %q", detail)
	}
	if !strings.Contains(detail, "claude-code install") {
		t.Errorf("detail = %q, want it to name claude-code install", detail)
	}
}

func TestDoctorDetailCurrentOnWSL(t *testing.T) {
	home := setHome(t)
	withProcVersion(t, "Linux version 5.15.90.1-microsoft-standard-WSL2 (...)\n")
	dest := filepath.Join(home, ".local", "bin", "wl-paste")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, ProxyScript, 0o755); err != nil {
		t.Fatal(err)
	}
	// PATH scoped to dest's own dir only — a real machine's wl-paste elsewhere on PATH must not
	// make exec.LookPath resolve ahead of the fixture and spuriously flip this to "not ok".
	t.Setenv("PATH", filepath.Dir(dest))
	ok, detail := DoctorDetail()
	if !ok {
		t.Errorf("current proxy on WSL: ok = false, detail %q", detail)
	}
	if !strings.Contains(detail, "claude-code clip") {
		t.Errorf("detail = %q, want it to name claude-code clip as the fallback", detail)
	}
}
