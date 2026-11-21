package pki

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func makeCert(t *testing.T, cn string, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) ([]byte, *x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	if parent == nil {
		parent, parentKey = tpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), cert, key
}

type fixture struct {
	get             func(string) string
	env             map[string]string
	rootPEM, issPEM []byte
}

func newFixture(t *testing.T) *fixture {
	rootPEM, root, rootKey := makeCert(t, "Test Root", nil, nil)
	issPEM, _, _ := makeCert(t, "Test Issuing", root, rootKey)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/pki/":
			w.Write([]byte(`<a href="/pki/a-root.pem">r</a><a href="/pki/a-issuing.pem">i</a>`))
		case "/pki/a-root.pem":
			w.Write(rootPEM)
		case "/pki/a-issuing.pem":
			w.Write(issPEM)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(root.Raw)
	pin := strings.ToUpper(hex.EncodeToString(sum[:]))
	env := map[string]string{envURL: srv.URL + "/pki/", envPin: pin, envRoot: `a-root\.pem`, envIss: `a-issuing\.pem`}
	return &fixture{get: func(k string) string { return env[k] }, env: env, rootPEM: rootPEM, issPEM: issPEM}
}

func TestPinnedBundle(t *testing.T) {
	fx := newFixture(t)
	sys := filepath.Join(t.TempDir(), "sys.pem")
	if err := os.WriteFile(sys, []byte("SYSTEM\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", sys)
	out := filepath.Join(t.TempDir(), "sub", "bundle.pem")
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), options{out: out}, fx.get, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, _ := os.ReadFile(out)
	if want := "SYSTEM\n" + string(fx.rootPEM) + string(fx.issPEM); string(got) != want {
		t.Errorf("bundle mismatch:\n%s", got)
	}
	if !strings.Contains(stdout.String(), "export SSL_CERT_FILE="+out) {
		t.Errorf("no export line: %q", stdout.String())
	}

	fx.env[envPin] = strings.Repeat("00", 32)
	err := run(context.Background(), options{out: out + "2"}, fx.get, &stdout, &stderr)
	wantErr(t, err, "refusing")
	if _, err := os.Stat(out + "2"); err == nil {
		t.Error("bundle written despite wrong pin")
	}
	err = run(context.Background(), options{out: out}, func(string) string { return "" }, &stdout, &stderr)
	wantErr(t, err, envURL, "vault env set")
}

func wantErr(t *testing.T, err error, subs ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	for _, s := range subs {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q lacks %q", err, s)
		}
	}
}

func siteFixture(t *testing.T) *fixture {
	fx := newFixture(t)
	t.Setenv("SSL_CERT_FILE", "")
	resolv := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(resolv, []byte("search example.test\nnameserver 192.0.2.10\nnameserver 192.0.2.11\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := resolvConf
	resolvConf = resolv
	t.Cleanup(func() { resolvConf = old })
	fx.env[envSites] = "eu=192.0.2.99=https://eu.example.test,us=192.0.2.11=https://us.example.test"
	fx.env[envPipRepo], fx.env[envProxy], fx.env[envNoProxy] = "pypi", "http://proxy.example.test:8080", "a.example.test,b.example.test"
	return fx
}

func TestSiteAndManagedBlock(t *testing.T) {
	fx := siteFixture(t)
	envFile := filepath.Join(t.TempDir(), ".env")
	foreign := "FOO=bar\n" + blockBegin + "\nexport STALE=1\n" + blockEnd + "\nBAZ=qux\n"
	if err := os.WriteFile(envFile, []byte(foreign), 0o640); err != nil {
		t.Fatal(err)
	}
	o := options{out: filepath.Join(t.TempDir(), "b.pem"), writeEnv: envFile}
	var stdout bytes.Buffer
	if err := run(context.Background(), o, fx.get, &stdout, &stdout); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(envFile)
	checkBlock(t, string(first), stdout.String())
	if err := run(context.Background(), o, fx.get, &stdout, &stdout); err != nil {
		t.Fatal(err)
	}
	if second, _ := os.ReadFile(envFile); !bytes.Equal(first, second) {
		t.Errorf("second run changed the file:\n%s", second)
	}
	if st, _ := os.Stat(envFile); st.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", st.Mode().Perm())
	}
	siteRefusals(t, fx, o)
}

func checkBlock(t *testing.T, first, stdout string) {
	for _, want := range []string{"FOO=bar\n", "BAZ=qux\n", "export PIP_INDEX_URL=https://us.example.test/repository/pypi/simple\n",
		`export NO_PROXY="a.example.test,b.example.test"`, "export HTTPS_PROXY=http://proxy.example.test:8080\n"} {
		if !strings.Contains(first, want) {
			t.Errorf("env file lacks %q:\n%s", want, first)
		}
	}
	if strings.Contains(first, "STALE") || !strings.Contains(stdout, "site: us") || strings.Contains(stdout, "proxy.example.test") {
		t.Errorf("block not replaced or stdout leaks values: %q", stdout)
	}
}

func siteRefusals(t *testing.T, fx *fixture, o options) {
	var sink bytes.Buffer
	o.site = "nope"
	wantErr(t, run(context.Background(), o, fx.get, &sink, &sink), "eu, us")
	o.site = ""
	fx.env[envSites] = "eu=192.0.2.99=https://eu.example.test,broken"
	wantErr(t, run(context.Background(), o, fx.get, &sink, &sink), "entry 2")
}
