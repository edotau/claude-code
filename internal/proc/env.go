package proc

import (
	"os"
	"runtime"
)

// Setenv sets a process-wide env var; process-wide state lives here, per-run values belong on the run.
func Setenv(key, value string) error { return os.Setenv(key, value) }

// Unsetenv unsets a process-wide env var; process-wide state lives here, per-run values belong on the run.
func Unsetenv(key string) error { return os.Unsetenv(key) }

// caBundles are the system CA bundle paths of the common Linux families, Debian first.
var caBundles = []string{"/etc/ssl/certs/ca-certificates.crt", "/etc/pki/tls/certs/ca-bundle.crt",
	"/etc/ssl/ca-bundle.pem", "/etc/ssl/cert.pem"}

// caVars are the CA-file variables runtimes read instead of the OS store (OpenSSL/Go, Python, curl, Node).
var caVars = []string{"SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "NODE_EXTRA_CA_CERTS"}

// FillCAEnv points each unset CA variable at the system bundle on Linux, so every child trusts what the OS trusts.
func FillCAEnv() { fillCAEnv(runtime.GOOS, caBundles) }

func fillCAEnv(goos string, bundles []string) {
	if goos != "linux" {
		return // macOS keeps corporate roots in the keychain, which no bundle file holds
	}
	for _, b := range bundles {
		if fi, err := os.Stat(b); err == nil && fi.Mode().IsRegular() {
			for _, v := range caVars {
				if os.Getenv(v) == "" {
					_ = Setenv(v, b)
				}
			}
			return
		}
	}
}
