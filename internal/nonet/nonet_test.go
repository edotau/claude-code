package nonet

import (
	"context"
	"net"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// A real name fails with ErrRealNetwork on any machine; localhost and IP literals still resolve.
func TestBlockRefusesRealDNS(t *testing.T) {
	_, err := net.DefaultResolver.LookupHost(context.Background(), "api.example.com")
	if err == nil || !strings.Contains(err.Error(), ErrRealNetwork.Error()) { // net.DNSError keeps only the text
		t.Fatalf("LookupHost(api.example.com) = %v, want ErrRealNetwork", err)
	}
	for _, host := range []string{"localhost", "127.0.0.1"} {
		if _, err := net.DefaultResolver.LookupHost(context.Background(), host); err != nil {
			t.Errorf("LookupHost(%s) = %v, want it to resolve", host, err)
		}
	}
	if !isTestBinary("/tmp/go-build1/b001/x.test") || !isTestBinary(`C:\b\x.test.exe`) || isTestBinary("claude-code") {
		t.Error("isTestBinary misreads a go test or production binary name")
	}
}

// Every package's test binary links nonet, so no test anywhere can reach a real API.
func TestEveryTestBinaryBlocksNetwork(t *testing.T) {
	out, err := exec.Command("go", "list", "-test", "-f", `{{.ImportPath}}|{{join .Deps " "}}`, "../../...").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	const self = "github.com/edotau/claude-code/internal/nonet"
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		pkg, deps, _ := strings.Cut(line, "|")
		if !strings.HasSuffix(pkg, ".test") || strings.HasPrefix(pkg, self) {
			continue
		}
		if !slices.Contains(strings.Fields(deps), self) {
			t.Errorf("%s: tests run without nonet; import it from a test file: _ %q", strings.TrimSuffix(pkg, ".test"), self)
		}
	}
}
