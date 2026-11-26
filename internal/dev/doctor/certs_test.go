package doctor

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/platform/paths"
)

func TestCABundleRowMissingIsOK(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	clearCAEnv(t)
	row := caBundleRow(time.Now())
	if row.Name != "ca bundle" || !row.OK || row.Hard || !strings.Contains(row.Detail, "none") {
		t.Fatalf("row = %+v, want an ok row that says none", row)
	}
	root := t.TempDir()
	blocked := filepath.Join(root, "not-a-dir")
	writeFile(t, blocked, []byte("x"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(blocked, "cfg"))
	if bad := caBundleRow(time.Now()); bad.OK || !strings.Contains(bad.Detail, "cannot stat") {
		t.Fatalf("row = %+v, want a failed stat", bad)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	if err := os.MkdirAll(paths.CABundle(), 0o755); err != nil {
		t.Fatal(err)
	}
	if dir := caBundleRow(time.Now()); dir.OK || !strings.Contains(dir.Detail, "not a file") {
		t.Fatalf("row = %+v, want not a file", dir)
	}
}

func TestCABundleRowFlagsNearExpiry(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	clearCAEnv(t)
	soon := time.Now().UTC().Add(10 * 24 * time.Hour).Truncate(time.Second)
	seedCorpCerts(t, soon, soon.Add(40*24*time.Hour))
	now := time.Now()
	row := caBundleRow(now)
	want := soon.Format("2006-01-02")
	if row.OK || row.Hard || !strings.Contains(row.Detail, want) || !strings.Contains(row.Detail, "4d old, ") {
		t.Fatalf("row = %+v, want not ok, %s and 4d old", row, want)
	}
	if !strings.HasSuffix(row.Detail, ", exported 0/4") {
		t.Fatalf("detail = %q, want exported 0/4", row.Detail)
	}
	for _, n := range paths.CAEnvNames {
		t.Setenv(n, paths.CABundle())
	}
	if !strings.HasSuffix(caBundleRow(now).Detail, ", exported") {
		t.Fatalf("detail = %q, want exported", caBundleRow(now).Detail)
	}
	t.Setenv("SSL_CERT_FILE", filepath.Join(dir, "other.pem"))
	writeFile(t, os.Getenv("SSL_CERT_FILE"), []byte("other"))
	if !strings.HasSuffix(caBundleRow(now).Detail, ", exported 3/4") {
		t.Fatalf("detail = %q, want exported 3/4", caBundleRow(now).Detail)
	}
}

func clearCAEnv(t *testing.T) {
	t.Helper()
	for _, n := range paths.CAEnvNames {
		t.Setenv(n, "")
	}
}

func seedCorpCerts(t *testing.T, soon, later time.Time) {
	t.Helper()
	writeTestCA(t, filepath.Join(paths.CertsDir(), "root.pem"), "Test Root", later)
	writeTestCA(t, filepath.Join(paths.CertsDir(), "issuing.pem"), "Test Issuing", soon)
	writeFile(t, paths.CABundle(), []byte("invented bundle"))
	old := time.Now().Add(-4*24*time.Hour - time.Hour)
	if err := os.Chtimes(paths.CABundle(), old, old); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeTestCA(t *testing.T, path, cn string, notAfter time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: notAfter.Add(-24 * time.Hour), NotAfter: notAfter,
		IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}
