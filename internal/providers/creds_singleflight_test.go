package providers

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// countingCmd returns an auth command that appends a line to a counter file, sleeps, then runs tail.
func countingCmd(t *testing.T, name, sleep, tail string) (cmd, counter string) {
	t.Helper()
	counter = filepath.Join(t.TempDir(), name+".count")
	return fmt.Sprintf("echo x >> %q; sleep %s; %s", counter, sleep, tail), counter
}

func runCount(t *testing.T, counter string) int {
	t.Helper()
	b, err := os.ReadFile(counter)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "x\n")
}

func freshProvider(t *testing.T, p *Provider) *Provider {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cmdCache.Delete(p.Name)
	t.Cleanup(func() { cmdCache.Delete(p.Name) })
	return p
}

// herd fires n concurrent Credential calls released together.
func herd(p *Provider, n int) ([]string, []error) {
	vals, errs := make([]string, n), make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			vals[i], errs[i] = Credential(context.Background(), p)
		}(i)
	}
	close(start)
	wg.Wait()
	return vals, errs
}

func TestCommandCredentialSingleFlightValue(t *testing.T) {
	cmd, counter := countingCmd(t, "sf", "0.5", "echo shared-token")
	p := freshProvider(t, &Provider{Name: "sf-value", Auth: Auth{Type: AuthBearer, Command: cmd}})
	vals, errs := herd(p, 20)
	for i := range vals {
		if errs[i] != nil || vals[i] != "shared-token" {
			t.Fatalf("caller %d: %q, %v", i, vals[i], errs[i])
		}
	}
	if c := runCount(t, counter); c != 1 {
		t.Fatalf("auth command ran %d times, want 1", c)
	}
}

func TestCommandCredentialSingleFlightSharesError(t *testing.T) {
	cmd, counter := countingCmd(t, "sfe", "0.5", "exit 3")
	p := freshProvider(t, &Provider{Name: "sf-err", Auth: Auth{Type: AuthBearer, Command: cmd}})
	_, errs := herd(p, 20)
	for i, err := range errs {
		if err == nil || !strings.Contains(err.Error(), "auth command failed") {
			t.Fatalf("caller %d: want shared auth failure, got %v", i, err)
		}
	}
	if c := runCount(t, counter); c != 1 {
		t.Fatalf("failing command ran %d times, want 1", c)
	}
	if _, ok := cmdCache.Load(p.Name); ok {
		t.Fatal("a failed mint must not be cached")
	}
}

func TestRefreshDuringInflightMintRunsForcedMint(t *testing.T) {
	cmd, counter := countingCmd(t, "rf", "1", "echo stale-token")
	forced := counter + ".forced"
	refresh := fmt.Sprintf("echo forced >> %q; echo fresh-token", forced)
	p := freshProvider(t, &Provider{Name: "sf-refresh",
		Auth: Auth{Type: AuthBearer, Command: cmd, RefreshCommand: refresh}})
	done := make(chan struct{})
	go func() { _, _ = Credential(context.Background(), p); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for runCount(t, counter) == 0 { // wait until the non-forced mint is truly in flight
		if time.Now().After(deadline) {
			t.Fatal("non-forced mint never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, err := Refresh(context.Background(), p)
	if err != nil || got != "fresh-token" {
		t.Fatalf("Refresh = %q, %v; want fresh-token (forced mint, not the in-flight result)", got, err)
	}
	if b, _ := os.ReadFile(forced); strings.Count(string(b), "forced") != 1 {
		t.Fatalf("refresh command ran %q, want once", b)
	}
	<-done
}

func TestAuthorizeStripsInboundAndSetsHeader(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cases := []struct{ name, typ, wantHdr, wantVal string }{
		{"apikey", AuthAPIKey, "X-Api-Key", "minted"},
		{"bearer", AuthBearer, "Authorization", "Bearer minted"},
		{"none", AuthNone, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &Provider{Name: "az-" + c.name, Auth: Auth{Type: c.typ, Command: "echo minted"}}
			cmdCache.Delete(p.Name)
			h := http.Header{}
			h.Set("Authorization", "Bearer client")
			h.Set("X-Api-Key", "client-key")
			h.Set("x-goog-api-key", "client-goog")
			if err := Authorize(context.Background(), p, h); err != nil {
				t.Fatal(err)
			}
			for _, k := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key"} {
				want := ""
				if strings.EqualFold(k, c.wantHdr) {
					want = c.wantVal
				}
				if got := h.Get(k); got != want {
					t.Errorf("%s = %q, want %q", k, got, want)
				}
			}
		})
	}
}

func TestCommandCredentialLeaderCancelDoesNotFailFollowers(t *testing.T) {
	cmd, counter := countingCmd(t, "lc", "0.6", "echo survived-token")
	p := freshProvider(t, &Provider{Name: "sf-cancel", Auth: Auth{Type: AuthBearer, Command: cmd}})
	lctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, _ = Credential(lctx, p) }() // leader
	deadline := time.Now().Add(5 * time.Second)
	for runCount(t, counter) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("leader mint never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	vals, errs := make([]string, 5), make([]error, 5)
	var wg sync.WaitGroup
	for i := range vals {
		wg.Add(1)
		go func(i int) { defer wg.Done(); vals[i], errs[i] = Credential(context.Background(), p) }(i)
	}
	time.Sleep(100 * time.Millisecond)
	cancel() // leader's ctx dies mid-mint
	wg.Wait()
	for i := range vals {
		if errs[i] != nil || vals[i] != "survived-token" {
			t.Fatalf("follower %d: %q, %v", i, vals[i], errs[i])
		}
	}
	if c := runCount(t, counter); c != 1 {
		t.Fatalf("auth command ran %d times, want 1", c)
	}
}

func TestMintOncePanicDoesNotWedgeLaterCallers(t *testing.T) {
	p := &Provider{Name: "flight-panic"}
	func() {
		defer func() { _ = recover() }()
		_, _ = mintOnce(context.Background(), p, true, func(context.Context) (string, error) { panic("boom") })
	}()
	done := make(chan string, 1)
	go func() {
		v, _ := mintOnce(context.Background(), p, true, func(context.Context) (string, error) { return "ok", nil })
		done <- v
	}()
	select {
	case v := <-done:
		if v != "ok" {
			t.Errorf("got %q", v)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second mintOnce hung after a panicking mint")
	}
}
