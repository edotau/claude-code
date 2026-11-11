// Package nonet makes a `go test` binary refuse real DNS lookups, so a test that reaches an API fails on every machine.
package nonet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// ErrRealNetwork is what a test's DNS lookup returns: localhost, /etc/hosts names and IP literals still work.
var ErrRealNetwork = errors.New("test made a real network call: mock the API (httptest server or the package's seam)")

func init() {
	if isTestBinary(os.Args[0]) {
		block()
	}
}

// isTestBinary reports a binary built by `go test` (pkg.test, pkg.test.exe).
func isTestBinary(arg0 string) bool {
	return strings.HasSuffix(strings.TrimSuffix(filepath.Base(arg0), ".exe"), ".test")
}

// block swaps the default resolver for one whose DNS server dial always fails; PreferGo keeps macOS off cgo DNS.
func block() {
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(_ context.Context, _, addr string) (net.Conn, error) {
		return nil, fmt.Errorf("%w (DNS via %s)", ErrRealNetwork, addr)
	}}
}
