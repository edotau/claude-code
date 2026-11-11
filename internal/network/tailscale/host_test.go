package tailscale

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/textutil"
)

// fakeTailscaleScript counts lookups, failing the first and succeeding thereafter.
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

// resetSelfCache isolates cache state and clock seams for each test.
func resetSelfCache(t *testing.T) {
	t.Helper()
	selfMu.Lock()
	oldReady, oldNames := selfReady, selfNames
	oldRetryAt, oldBackoff, oldNow := selfRetryAt, selfFailureBackoff, now
	selfRetryAt = time.Time{}
	selfReady, selfNames = false, nil
	selfMu.Unlock()
	t.Cleanup(func() {
		selfMu.Lock()
		selfReady, selfNames = oldReady, oldNames
		selfRetryAt, selfFailureBackoff, now = oldRetryAt, oldBackoff, oldNow
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

func TestIsLocalHost_FailedLookupNotCachedSuccessIs(t *testing.T) {
	resetSelfCache(t)
	counter := fakeTailscaleScript(t)
	clock := time.Now()
	now = func() time.Time { return clock }
	selfFailureBackoff = time.Second
	ctx := context.Background()
	if IsLocalHost(ctx, "test-host") {
		t.Fatal("call 1: failed lookup returned true")
	}
	if calls := readCalls(t, counter); calls != "1\n" {
		t.Fatalf("call 1: invocations = %q, want 1", calls)
	}
	if IsLocalHost(ctx, "test-host") {
		t.Fatal("call 2: backoff returned true")
	}
	if calls := readCalls(t, counter); calls != "1\n" {
		t.Fatalf("call 2: invocations = %q, want 1", calls)
	}
	clock = clock.Add(selfFailureBackoff + time.Nanosecond)
	if !IsLocalHost(ctx, "test-host") {
		t.Fatal("call 3: retry returned false")
	}
	if calls := readCalls(t, counter); calls != "2\n" {
		t.Fatalf("call 3: invocations = %q, want 2", calls)
	}
	if !IsLocalHost(ctx, "test-host") {
		t.Fatal("call 4: cached success returned false")
	}
	if calls := readCalls(t, counter); calls != "2\n" {
		t.Fatalf("call 4: invocations = %q, want 2", calls)
	}
}

func TestIsLocalHost_ObviousNamesSkipTailscale(t *testing.T) {
	resetSelfCache(t)
	counter := fakeTailscaleScript(t)
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	short, _, _ := strings.Cut(hostname, ".")
	for _, host := range []string{"localhost", "127.0.0.1", "::1", hostname, strings.ToUpper(hostname), short} {
		if !IsLocalHost(context.Background(), host) {
			t.Errorf("IsLocalHost(%q) = false", host)
		}
	}
	if calls := readCalls(t, counter); calls != "0" {
		t.Fatalf("local names invoked tailscale: %q", calls)
	}
}

func TestIsLocalHost_SlowLookupDoesNotBlock(t *testing.T) {
	resetSelfCache(t)
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	release := filepath.Join(dir, "release")
	script := "#!/bin/sh\necho started > " + textutil.ShellQuote(started) + "\nwhile [ ! -e " +
		textutil.ShellQuote(release) + " ]; do sleep 0.02; done\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "tailscale"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	done := make(chan bool, 1)
	go func() { done <- IsLocalHost(context.Background(), "remote-test-host") }()
	defer func() {
		_ = os.WriteFile(release, nil, 0o600)
		<-done
	}()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("tailscale did not start")
		}
		time.Sleep(time.Millisecond)
	}
	for _, host := range []string{"localhost", "another-remote-host"} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result := make(chan bool, 1)
		go func() { result <- IsLocalHost(ctx, host) }()
		select {
		case local := <-result:
			if local != (host == "localhost") {
				t.Errorf("IsLocalHost(%q) = %v", host, local)
			}
		case <-time.After(100 * time.Millisecond):
			<-result
			t.Fatalf("IsLocalHost(%q) blocked behind slow lookup", host)
		}
	}
}

func TestResolveHost_RejectsOptionShapedHost(t *testing.T) {
	for _, h := range []string{"-oProxyCommand=touch /tmp/x", "-v", "", "a b"} {
		if got, err := ResolveHost(context.Background(), h); err == nil {
			t.Errorf("ResolveHost(%q) = %q, want error", h, got)
		}
	}
}

func TestMatchMachine(t *testing.T) {
	names := []string{"", "wks.tail.ts.net", "WKS.other.ts.net", "bare"}
	for host, want := range map[string]string{"wks": "wks.tail.ts.net", "WKS.OTHER.TS.NET": "WKS.other.ts.net",
		"Bare": "bare"} {
		if got, ok := matchMachine(names, host); !ok || got != want {
			t.Errorf("MatchMachine(%q) = %q, %v; want %q", host, got, ok, want)
		}
	}
	for _, host := range []string{"tail", "wks.tail", "other"} {
		if got, ok := matchMachine(names, host); ok {
			t.Errorf("MatchMachine(%q) = %q, want no match", host, got)
		}
	}
}

// countingCLI stubs the tailscale CLI with the running fixture and returns a func reporting how often it ran.
func countingCLI(t *testing.T) func() int {
	t.Helper()
	hermeticSecrets(t)
	calls := filepath.Join(t.TempDir(), "calls")
	stubCLI(t, "echo x >> "+shQuote(calls)+"\n"+catScript(t, "status_running.json"))
	return func() int {
		b, _ := os.ReadFile(calls)
		return strings.Count(string(b), "x")
	}
}

// TestResolveHost_SkipsLookupAndPrefersCache: FQDNs and IPs never look up; a cached short name never runs the CLI.
func TestResolveHost_SkipsLookupAndPrefersCache(t *testing.T) {
	calls := countingCLI(t)
	ctx := context.Background()
	WriteMachinesCache([]string{"box.tailxxxxx.ts.net"})
	for host, want := range map[string]string{"other.example.com": "other.example.com", "100.64.1.2": "100.64.1.2",
		"fd7a::1": "fd7a::1", "BOX": "box.tailxxxxx.ts.net"} {
		if got, err := ResolveHost(ctx, host); err != nil || got != want {
			t.Errorf("ResolveHost(%q) = %q, %v; want %q", host, got, err, want)
		}
	}
	if n := calls(); n != 0 {
		t.Fatalf("tailscale CLI ran %d times, want 0 (no lookup for FQDN/IP, cache hit for BOX)", n)
	}
	// A cache miss goes live once and refreshes the cache with the live listing.
	if got, _ := ResolveHost(ctx, "test-phone"); got != "test-phone.tailxxxxx.ts.net." || calls() != 1 {
		t.Errorf("live ResolveHost(test-phone) = %q after %d CLI calls, want the MagicDNS name after 1", got, calls())
	}
	if !slices.Contains(CachedMachineNames(), "test-phone.tailxxxxx.ts.net.") {
		t.Errorf("cache after live lookup = %q, want test-phone listed", CachedMachineNames())
	}
}

// writeCacheAt plants a cache file whose timestamp is at, in a hermetic config dir.
func writeCacheAt(t *testing.T, at time.Time) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	data, err := json.Marshal(machinesCache{Schema: machinesCacheSchema, At: at, Names: []string{"h1"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(machinesCachePath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(machinesCachePath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A fresh entry is served; one stamped in the future (clock skew) is stale, never immortal.
func TestCachedMachineNamesFreshnessWindow(t *testing.T) {
	writeCacheAt(t, time.Now().Add(-time.Minute))
	if got := CachedMachineNames(); len(got) != 1 {
		t.Errorf("fresh cache = %v, want [h1]", got)
	}
	writeCacheAt(t, time.Now().Add(24*time.Hour))
	if got := CachedMachineNames(); got != nil {
		t.Errorf("future-dated cache = %v, want nil", got)
	}
}
