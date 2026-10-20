package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// fakeTailscaleScript installs a `tailscale` on PATH that fails its first invocation (simulating a
// down tailnet daemon) and succeeds every one after, counting invocations in a side file so the test
// can tell whether IsLocalHost re-ran the lookup or served it from cache.
func fakeTailscaleScript(t *testing.T) (counter string) {
	t.Helper()
	dir := t.TempDir()
	counter = filepath.Join(dir, "calls")
	if err := os.WriteFile(counter, []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
n=$(cat "` + counter + `")
n=$((n + 1))
echo "$n" > "` + counter + `"
if [ "$n" -eq 1 ]; then
  exit 1
fi
cat <<'EOF'
{"BackendState":"Running","Self":{"DNSName":"test-host.tailnetxyz.ts.net.","TailscaleIPs":["100.100.100.100"]}}
EOF
`
	bin := filepath.Join(dir, "tailscale")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return counter
}

// resetSelfCache snapshots and restores the package-level self* cache around a test that exercises
// IsLocalHost's own caching, so it doesn't leak into/out of other tests sharing this package.
func resetSelfCache(t *testing.T) {
	t.Helper()
	selfMu.Lock()
	oldReady, oldNames := selfReady, selfNames
	selfReady, selfNames = false, nil
	selfMu.Unlock()
	t.Cleanup(func() {
		selfMu.Lock()
		selfReady, selfNames = oldReady, oldNames
		selfMu.Unlock()
	})
}

func readCalls(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A failed tailscale lookup must not be cached: the first IsLocalHost call sees the daemon down and
// returns false for a tailnet-only host; a later call must retry the lookup and can then succeed. A
// successful lookup, once made, IS cached: a third call must not re-invoke `tailscale` at all.
func TestIsLocalHost_FailedLookupNotCachedSuccessIs(t *testing.T) {
	resetSelfCache(t)
	counter := fakeTailscaleScript(t)
	ctx := context.Background()

	if got := IsLocalHost(ctx, "test-host"); got {
		t.Fatalf("IsLocalHost = true on the first (failed-lookup) call, want false")
	}
	if calls := readCalls(t, counter); calls != "1\n" {
		t.Fatalf("tailscale invoked %s times after call 1, want 1", calls)
	}

	if got := IsLocalHost(ctx, "test-host"); !got {
		t.Fatalf("IsLocalHost = false on the retry call, want true (the failed lookup must not have been cached)")
	}
	if calls := readCalls(t, counter); calls != "2\n" {
		t.Fatalf("tailscale invoked %s times after call 2, want 2 (the retry)", calls)
	}

	if got := IsLocalHost(ctx, "test-host"); !got {
		t.Fatalf("IsLocalHost = false on the third call, want true")
	}
	if calls := readCalls(t, counter); calls != "2\n" {
		t.Fatalf("tailscale invoked %s times after call 3, want still 2 (the success must be cached)", calls)
	}
}
