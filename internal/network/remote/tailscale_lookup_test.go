package remote

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	script := "#!/bin/sh\necho started > " + QuoteSH(started) + "\nwhile [ ! -e " + QuoteSH(release) + " ]; do sleep 0.02; done\nexit 1\n"
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
