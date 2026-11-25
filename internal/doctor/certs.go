package doctor

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
)

const caExpiryWarn = 30 * 24 * time.Hour

// caBundleRow reports the corporate CA bundle's age, its earliest corp CA expiry and whether this process exported it.
func caBundleRow(now time.Time) check {
	row := check{Name: "ca bundle", OK: true}
	fi, err := os.Stat(paths.CABundle())
	if err != nil {
		if os.IsNotExist(err) {
			row.Detail = "none (claude-code network pki writes state/certs)"
			return row
		}
		row.OK = false
		row.Detail = textutil.TruncateCells("cannot stat ca bundle: "+err.Error(), 80-len("ca bundle: "), "…")
		return row
	}
	if fi.IsDir() {
		row.OK, row.Detail = false, "corp-ca-bundle.pem is not a file"
		return row
	}
	exp, err := earliestExpiry(paths.CertsDir(), "root.pem", "issuing.pem")
	if err != nil {
		row.OK = false
		row.Detail = textutil.TruncateCells("cannot read corp CA: "+err.Error(), 80-len("ca bundle: "), "…")
		return row
	}
	days := int(now.Sub(fi.ModTime()).Hours() / 24)
	row.OK = exp.Sub(now) > caExpiryWarn
	row.Detail = fmt.Sprintf("state/certs, %dd old, CA expires %s%s", days, exp.UTC().Format("2006-01-02"), caExportNote())
	return row
}

// caExportNote says how many CA variables this process exported to CABundle.
func caExportNote() string {
	n, bundle := 0, paths.CABundle()
	for _, name := range paths.CAEnvNames {
		if sameFile(os.Getenv(name), bundle) {
			n++
		}
	}
	if n == len(paths.CAEnvNames) {
		return ", exported"
	}
	return fmt.Sprintf(", exported %d/%d", n, len(paths.CAEnvNames))
}

// sameFile reports whether a and b name one file; a missing path is not the bundle.
func sameFile(a, b string) bool {
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ia, ib)
}

// earliestExpiry is the soonest NotAfter among the first certificate of each named PEM file in dir.
func earliestExpiry(dir string, names ...string) (time.Time, error) {
	var first time.Time
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return time.Time{}, err
		}
		block, _ := pem.Decode(data)
		if block == nil {
			return time.Time{}, errors.New(n + ": no PEM block")
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return time.Time{}, err
		}
		if first.IsZero() || c.NotAfter.Before(first) {
			first = c.NotAfter
		}
	}
	return first, nil
}
