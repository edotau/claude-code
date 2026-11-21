// Package pki implements `network pki`: a pinned corporate CA bundle whose values come only from the environment.
package pki

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/httpx"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault"
)

const (
	envURL, envPin  = "PKI_URL", "PKI_ROOT_SHA256"
	envRoot, envIss = "PKI_ROOT_PATTERN", "PKI_ISSUING_PATTERN"
	envUA           = "PKI_USER_AGENT"
	debianTrustDir  = "/usr/local/share/ca-certificates/corp"
	rhelTrustDir    = "/etc/pki/ca-trust/source/anchors"
	maxBody         = 4 << 20
)

var requiredKeys = []string{envURL, envPin, envRoot, envIss}

// Bound every fetch and system command so a stalled server or hung systemctl fails the run.
var (
	fetchTimeout = 60 * time.Second
	cmdTimeout   = 2 * time.Minute
)

var hrefRe = regexp.MustCompile(`(?i)href="([^"]+)"`)

type options struct {
	out, nodeServices string
	site, writeEnv    string
	trust             bool
}

func addFlags(fs *flag.FlagSet) *options {
	o := &options{}
	fs.StringVar(&o.out, "out", "", "bundle path to write (default: the user data dir, certs/corp-ca-bundle.pem)")
	fs.BoolVar(&o.trust, "trust", false, "also install root and issuing CA into the system trust store (Linux, root)")
	fs.StringVar(&o.nodeServices, "node-services", "", "comma-separated systemd services to point NODE_EXTRA_CA_CERTS at the bundle")
	fs.StringVar(&o.site, "site", "", "force a site by name from "+envSites+" instead of detecting it from resolv.conf")
	fs.StringVar(&o.writeEnv, "write-env", "", "rewrite only the managed pki block in this env file (created if missing)")
	return o
}

// Flags are the completion flags of `network pki`, derived from its FlagSet.
func Flags() []helptext.Flag {
	fs := helptext.NewFlagSet("network pki", helptext.Page{})
	addFlags(fs)
	return helptext.FlagsOf(fs, nil)
}

// Cmd builds the pinned bundle and prints the export lines; --trust and --node-services need root on Linux.
func Cmd(args []string) int {
	fs := helptext.NewFlagSet("network pki", helptext.Page{
		Usage: "claude-code network pki [--out <path>] [--site <name>] [--write-env <file>] [--trust] [--node-services a,b]",
		About: "Fetch the corporate root and issuing CA, pin the root by SHA-256, verify the chain and write a CA bundle.\n" +
			"Required (process env, `claude-code vault env set`, or ./.env): " + strings.Join(requiredKeys, ", ") + ".\n" +
			"Optional: " + envUA + ", " + envSites + " (" + sitesShape + ", comma list), " + envPipRepo + ", " + envProxy + ", " + envNoProxy + ". Values are never printed."})
	o := addFlags(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	ctx := context.Background()
	if err := run(ctx, *o, lookupEnv(), os.Stdout, os.Stderr); err != nil {
		return helptext.Fail("network pki: %v", err)
	}
	return 0
}

// lookupEnv reads process env, the vault env bucket (refs resolved), a sealed secret of that name, then ./.env.
func lookupEnv() func(string) string {
	sealed := func(k string) (string, bool) {
		v, _, err := vault.Lookup(k)
		return v, err == nil && v != ""
	}
	vaultEnv, _, _ := providers.ResolveCustomEnv(sealed)
	dotenv := map[string]string{}
	if f, err := os.Open(".env"); err == nil {
		pairs, _ := vault.ParseDotenv(f)
		_ = f.Close()
		for _, p := range pairs {
			dotenv[p.Name] = vault.UnquoteDotenv(p.Value)
		}
	}
	return func(k string) string {
		for _, v := range []string{os.Getenv(k), vaultEnv[k]} {
			if v != "" {
				return v
			}
		}
		if v, ok := sealed(k); ok {
			return v
		}
		return dotenv[k]
	}
}

type config struct {
	url, pin, ua string
	root, issuer *regexp.Regexp
}

func loadConfig(get func(string) string) (config, error) {
	var missing []string
	for _, k := range requiredKeys {
		if get(k) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return config{}, fmt.Errorf("missing %s: set them with `claude-code vault env set` or in ./.env", strings.Join(missing, ", "))
	}
	c := config{url: get(envURL), pin: normalizePin(get(envPin)), ua: get(envUA)}
	var err error
	if c.root, err = regexp.Compile(get(envRoot)); err != nil {
		return c, fmt.Errorf("%s is not a valid regexp: %w", envRoot, err)
	}
	if c.issuer, err = regexp.Compile(get(envIss)); err != nil {
		return c, fmt.Errorf("%s is not a valid regexp: %w", envIss, err)
	}
	return c, nil
}

func normalizePin(s string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), ":", ""))
}

func run(ctx context.Context, o options, get func(string) string, w, errw io.Writer) error {
	cfg, err := loadConfig(get)
	if err != nil {
		return err
	}
	if runtime.GOOS != "linux" && (o.trust || o.nodeServices != "") {
		return errors.New("--trust and --node-services are Linux only")
	}
	out, err := outPath(o.out)
	if err != nil {
		return err
	}
	st, extra, err := siteEnv(get, o.site)
	if err != nil {
		return err
	}
	rootPEM, issPEM, err := fetchPinned(ctx, cfg, errw)
	if err != nil {
		return err
	}
	if err := writeBundle(out, rootPEM, issPEM); err != nil {
		return err
	}
	if err := report(o, out, st, extra, w); err != nil {
		return err
	}
	return applySystem(ctx, o, out)
}

// report prints the export lines, or with --write-env writes the block and prints the site and key names only.
func report(o options, out string, st *site, extra []kv, w io.Writer) error {
	if o.writeEnv == "" {
		fmt.Fprintf(w, "export SSL_CERT_FILE=%[1]s REQUESTS_CA_BUNDLE=%[1]s CURL_CA_BUNDLE=%[1]s NODE_EXTRA_CA_CERTS=%[1]s\n", out)
		for _, p := range extra {
			fmt.Fprintln(w, exportLine(p))
		}
		return nil
	}
	file, err := filepath.Abs(paths.ExpandHome(o.writeEnv))
	if err != nil {
		return err
	}
	pairs := append(caPairs(out), extra...)
	if err := writeEnvBlock(file, pairs); err != nil {
		return err
	}
	name := "none"
	if st != nil {
		name = st.name
	}
	keys := make([]string, len(pairs))
	for i, p := range pairs {
		keys[i] = p.key
	}
	fmt.Fprintf(w, "site: %s\nwrote %s: %s\n", name, file, strings.Join(keys, " "))
	return nil
}

func applySystem(ctx context.Context, o options, out string) error {
	if o.trust {
		if err := installTrust(ctx, filepath.Dir(out)); err != nil {
			return err
		}
	}
	for svc := range strings.SplitSeq(o.nodeServices, ",") {
		if svc = strings.TrimSpace(svc); svc == "" {
			continue
		}
		if err := nodeDropin(ctx, svc, out); err != nil {
			return err
		}
	}
	return nil
}

func outPath(flagVal string) (string, error) {
	if flagVal != "" {
		return filepath.Abs(paths.ExpandHome(flagVal))
	}
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "claude-code", "certs", "corp-ca-bundle.pem"), nil
}

// fetchPinned downloads the root and issuing CA and returns them only if the root pin and chain both check out.
func fetchPinned(ctx context.Context, cfg config, errw io.Writer) (rootPEM, issPEM []byte, err error) {
	u, err := url.Parse(cfg.url)
	if err != nil || u.Host == "" {
		return nil, nil, fmt.Errorf("%s is not a valid URL", envURL)
	}
	f := &fetcher{ua: cfg.ua, errw: errw}
	index, err := f.get(ctx, cfg.url)
	if err != nil {
		return nil, nil, fmt.Errorf("index: %w", err)
	}
	var certs [2]*x509.Certificate
	var pems [2][]byte
	for i, ptn := range []*regexp.Regexp{cfg.root, cfg.issuer} {
		label := [2]string{"root", "issuing"}[i]
		href := findHref(string(index), u.Path, ptn)
		if href == "" {
			return nil, nil, fmt.Errorf("%s CA not found in the index", label)
		}
		if pems[i], err = f.get(ctx, strings.TrimRight(cfg.url, "/")+"/"+strings.TrimPrefix(href, "/")); err != nil {
			return nil, nil, fmt.Errorf("%s CA: %w", label, err)
		}
		if certs[i], pems[i], err = parseCert(pems[i]); err != nil {
			return nil, nil, fmt.Errorf("%s CA: %w", label, err)
		}
	}
	if err := checkChain(certs[0], certs[1], cfg.pin); err != nil {
		return nil, nil, err
	}
	return pems[0], pems[1], nil
}

// findHref returns the first href part after basePath that matches ptn.
func findHref(html, basePath string, ptn *regexp.Regexp) string {
	for _, m := range hrefRe.FindAllStringSubmatch(html, -1) {
		if _, after, ok := strings.Cut(m[1], basePath); ok && ptn.MatchString(after) {
			return after
		}
	}
	return ""
}

func parseCert(data []byte) (*x509.Certificate, []byte, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, nil, errors.New("not a PEM certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("not a valid X.509 certificate: %w", err)
	}
	return cert, pem.EncodeToMemory(block), nil
}

func checkChain(root, issuing *x509.Certificate, pin string) error {
	sum := sha256.Sum256(root.Raw)
	if got := hex.EncodeToString(sum[:]); got != pin {
		return errors.New("root CA SHA-256 does not match " + envPin + "; refusing to trust it")
	}
	pool := x509.NewCertPool()
	pool.AddCert(root)
	opts := x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}
	if _, err := issuing.Verify(opts); err != nil {
		return fmt.Errorf("issuing CA does not chain to the pinned root; refusing to trust it: %w", err)
	}
	return nil
}

// fetcher verifies TLS first and drops to unverified once if that fails; the pin makes the fallback safe.
type fetcher struct {
	ua       string
	errw     io.Writer
	insecure bool
}

func (f *fetcher) get(ctx context.Context, rawURL string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	body, err := f.once(ctx, rawURL)
	var verr *tls.CertificateVerificationError
	if errors.As(err, &verr) && !f.insecure {
		fmt.Fprintln(f.errw, "network pki: TLS verification of the PKI host failed; retrying unverified (root pin still enforced)")
		f.insecure = true
		body, err = f.once(ctx, rawURL)
	}
	return body, err
}

func (f *fetcher) once(ctx context.Context, rawURL string) ([]byte, error) {
	client := &http.Client{Timeout: fetchTimeout, CheckRedirect: httpx.SameOriginRedirect}
	if f.insecure {
		t := http.DefaultTransport.(*http.Transport).Clone()      // keeps HTTPS_PROXY/NO_PROXY on the fallback
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // pinned
		client.Transport = t
	}
	hdr := http.Header{}
	if f.ua != "" {
		hdr.Set("User-Agent", f.ua)
	}
	status, body, _, err := httpx.Do(ctx, httpx.Call{Client: client, URL: rawURL, Header: hdr, Limit: maxBody})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", rawURL, status)
	}
	return body, nil
}

// systemBundle skips SSL_CERT_FILE when it is our own output, else each re-run after `source .env` re-appends the CAs.
func systemBundle(out string) []byte {
	paths := []string{os.Getenv("SSL_CERT_FILE"), "/etc/ssl/certs/ca-certificates.crt", "/etc/pki/tls/certs/ca-bundle.crt", "/etc/ssl/cert.pem"}
	for _, p := range paths {
		if p == "" || sameFile(p, out) {
			continue
		}
		if data, err := os.ReadFile(p); err == nil {
			return data
		}
	}
	return nil
}

func sameFile(a, b string) bool {
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ia, ib)
}

// writeBundle writes root.pem, issuing.pem and the system bundle plus both CAs beside out.
func writeBundle(out string, rootPEM, issPEM []byte) error {
	dir := filepath.Dir(out)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	bundle := systemBundle(out)
	if len(bundle) > 0 && !bytes.HasSuffix(bundle, []byte("\n")) {
		bundle = append(bundle, '\n')
	}
	bundle = append(append(bundle, rootPEM...), issPEM...)
	for name, data := range map[string][]byte{"root.pem": rootPEM, "issuing.pem": issPEM} {
		if err := paths.AtomicWrite(filepath.Join(dir, name), data, 0o644); err != nil {
			return err
		}
	}
	return paths.AtomicWrite(out, bundle, 0o644)
}

func needRoot(what string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("%s needs sudo (root)", what)
	}
	return nil
}

func installTrust(ctx context.Context, dir string) error {
	if err := needRoot("--trust"); err != nil {
		return err
	}
	dst, refresh := debianTrustDir, "update-ca-certificates"
	if !paths.IsDir(filepath.Dir(debianTrustDir)) {
		if !paths.IsDir(rhelTrustDir) {
			return errors.New("no known system trust store directory found")
		}
		dst, refresh = rhelTrustDir, "update-ca-trust"
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, name := range []string{"root", "issuing"} {
		data, err := os.ReadFile(filepath.Join(dir, name+".pem"))
		if err != nil {
			return err
		}
		if err := paths.AtomicWrite(filepath.Join(dst, "corp-"+name+".crt"), data, 0o644); err != nil {
			return err
		}
	}
	return runCmd(ctx, refresh)
}

func nodeDropin(ctx context.Context, service, bundle string) error {
	if err := needRoot("--node-services"); err != nil {
		return err
	}
	dir := "/etc/systemd/system/" + service + ".service.d"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	body := "[Service]\nEnvironment=NODE_EXTRA_CA_CERTS=" + bundle + "\nPassEnvironment=NODE_EXTRA_CA_CERTS\n"
	if err := paths.AtomicWrite(filepath.Join(dir, "ca-trust.conf"), []byte(body), 0o644); err != nil {
		return err
	}
	for _, args := range [][]string{{"daemon-reload"}, {"restart", service}, {"is-active", "--quiet", service}} {
		if err := runCmd(ctx, "systemctl", args...); err != nil {
			return err
		}
	}
	return nil
}

func runCmd(ctx context.Context, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, cmdTimeout)
	defer cancel()
	if out, err := exec.CommandContext(ctx, name, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
