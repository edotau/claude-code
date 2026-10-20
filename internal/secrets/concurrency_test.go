package secrets

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// The subprocess inherits the parent's t.TempDir-backed vault and master key.
func TestConcurrentStoreHelperStoreOneValue(t *testing.T) {
	name := os.Getenv("CONCURRENCY_HELPER_NAME")
	if name == "" {
		t.Skip("not invoked as the concurrency helper")
	}
	if err := Store(name, "v-"+name); err != nil {
		t.Fatalf("Store(%s): %v", name, err)
	}
}

func TestConcurrentStoreAllLand(t *testing.T) {
	vaulttest.Use(t)
	const processes = 8
	names := make([]string, processes)
	for i := range names {
		names[i] = fmt.Sprintf("CONC_%02d", i)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, len(names))
	for i, n := range names {
		wg.Add(1)
		go func(i int, n string) {
			defer wg.Done()
			<-start
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestConcurrentStoreHelperStoreOneValue$", "-test.v")
			cmd.Env = append(os.Environ(),
				"CONCURRENCY_HELPER_NAME="+n,
			)
			out, err := cmd.CombinedOutput()
			if err != nil {
				errs[i] = err
				t.Logf("helper %s output:\n%s", n, out)
			}
		}(i, n)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("helper %s failed: %v", names[i], err)
		}
	}

	if got := Names(); !slices.Equal(got, names) {
		t.Fatalf("Names = %v, want %v", got, names)
	}
	for _, n := range names {
		if value, source, err := Lookup(n); err != nil || value != "v-"+n || source != "vault" {
			t.Errorf("Lookup(%s) = (%q, %q, %v), want (%q, vault, nil)", n, value, source, err, "v-"+n)
		}
	}

	m, err := LoadMeta()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if _, ok := m[n]; !ok {
			t.Errorf("vault metadata missing a row for %s", n)
		}
	}
}
