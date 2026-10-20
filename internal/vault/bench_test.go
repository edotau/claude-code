package vault

import "testing"

// BenchmarkViewEmpty and BenchmarkUpdateSmall measure the cost the retired goroutine-stack-parsing
// nested-open guard (enterTx/exitTx/goroutineID, a sync.Map plus two runtime.Stack captures per
// call) added to every View/Update; run before vs after its removal to show the benefit (R-review
// nested-open-guard decision).
func BenchmarkViewEmpty(b *testing.B) {
	phase1bBenchRing(b)
	for b.Loop() {
		if err := View(func(*Tx) error { return nil }); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkUpdateSmall(b *testing.B) {
	phase1bBenchRing(b)
	for b.Loop() {
		if err := Update(func(tx *Tx) error { return tx.Put(Pins, "k", "v") }); err != nil {
			b.Fatal(err)
		}
	}
}

func phase1bBenchRing(b *testing.B) {
	b.Helper()
	b.Setenv("CLAUDE_CONFIG_DIR", b.TempDir())
	b.Setenv("HARNESS_VAULT_KEY", "")
	ring := &specKeyring{values: make(map[string]string)}
	restore := UseKeyringSeams(ring.lookup, ring.set, ring.delete)
	b.Cleanup(restore)
}
