package proc

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSetenvUnsetenv(t *testing.T) {
	const key = "PROC_ENV_WRAPPER_TEST"
	t.Setenv(key, "")
	if err := Setenv(key, "v"); err != nil || os.Getenv(key) != "v" {
		t.Fatalf("Setenv: err=%v value=%q", err, os.Getenv(key))
	}
	if err := Unsetenv(key); err != nil {
		t.Fatalf("Unsetenv: %v", err)
	}
	if _, ok := os.LookupEnv(key); ok {
		t.Fatal("key still set after Unsetenv")
	}
}

func TestFillCAEnv(t *testing.T) {
	bundle := filepath.Join(t.TempDir(), "bundle.crt")
	if err := os.WriteFile(bundle, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, v := range caVars {
		t.Setenv(v, "")
	}
	t.Setenv("NODE_EXTRA_CA_CERTS", "/mine.pem")
	fillCAEnv("darwin", []string{bundle})
	if os.Getenv("SSL_CERT_FILE") != "" {
		t.Fatal("non-Linux must set nothing")
	}
	fillCAEnv("linux", []string{filepath.Join(t.TempDir(), "missing"), bundle})
	if os.Getenv("SSL_CERT_FILE") != bundle || os.Getenv("REQUESTS_CA_BUNDLE") != bundle {
		t.Fatalf("unset vars not filled: %q", os.Getenv("SSL_CERT_FILE"))
	}
	if os.Getenv("NODE_EXTRA_CA_CERTS") != "/mine.pem" {
		t.Fatal("a set value must win")
	}
}
