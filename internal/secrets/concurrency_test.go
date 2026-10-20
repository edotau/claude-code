package secrets

import (
	"os"
	"os/exec"
	"sync"
	"testing"
)

// TestConcurrentStoreHelperStoreOneValue is re-exec'd as a subprocess by TestConcurrentStoreBothLand;
// it stores one name under the shared CLAUDE_CONFIG_DIR, file plane only, then exits.
func TestConcurrentStoreHelperStoreOneValue(t *testing.T) {
	name := os.Getenv("CONCURRENCY_HELPER_NAME")
	if name == "" {
		t.Skip("not invoked as the concurrency helper")
	}
	if _, err := Store(name, "v-"+name, true); err != nil {
		t.Fatalf("Store(%s): %v", name, err)
	}
}

// Two processes storing different names against the same CLAUDE_CONFIG_DIR (file plane, forceFile)
// must both land: both rows in the meta file, both lines in secrets.env.
func TestConcurrentStoreBothLand(t *testing.T) {
	dir := setConfigDir(t)
	names := []string{"CONC_A", "CONC_B"}

	var wg sync.WaitGroup
	errs := make([]error, len(names))
	for i, n := range names {
		wg.Add(1)
		go func(i int, n string) {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=TestConcurrentStoreHelperStoreOneValue", "-test.v")
			cmd.Env = append(os.Environ(),
				"CONCURRENCY_HELPER_NAME="+n,
				"CLAUDE_CONFIG_DIR="+dir,
			)
			out, err := cmd.CombinedOutput()
			if err != nil {
				errs[i] = err
				t.Logf("helper %s output:\n%s", n, out)
			}
		}(i, n)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("helper %s failed: %v", names[i], err)
		}
	}

	file := ReadAll()
	for _, n := range names {
		if file[n] != "v-"+n {
			t.Errorf("secrets.env missing %s=%s, got %q", n, "v-"+n, file[n])
		}
	}

	m, err := LoadMeta()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if _, ok := m[n]; !ok {
			t.Errorf("meta file missing a row for %s", n)
		}
	}
}
