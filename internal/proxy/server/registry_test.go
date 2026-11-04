package server

import (
	"io"
	"os"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// bumpMtime sets an exact timestamp to exercise second-granularity registry comparisons.
func bumpMtime(t *testing.T, path string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

// Failed registry loads must not advance the cached timestamp or hide a later fix at the same timestamp.
func TestRegistryRetriesLoadAfterAFailureRatherThanCachingIt(t *testing.T) {
	vaulttest.Use(t)
	const good = `{"providers":{}}`
	vaulttest.Put(t, vault.Overlay, "providers", good)
	r := newRouter(secret)
	r.log = io.Discard

	reg1, err := r.registry()
	if err != nil {
		t.Fatalf("first registry() = %v", err)
	}
	stamp1 := r.regStamp
	badStamp := stamp1.Add(time.Second)

	// Corrupt the overlay and force the vault file's mtime forward so registry() sees a change.
	vaulttest.Put(t, vault.Overlay, "providers", "not json")
	bumpMtime(t, vault.Path(), badStamp)

	reg2, err := r.registry()
	if err != nil {
		t.Fatalf("second registry() = %v, want it to keep the previous registry on a bad overlay", err)
	}
	if reg2 != reg1 {
		t.Error("a failed Load must keep serving the previous *Registry, not swap in a different one")
	}
	if r.regStamp.Equal(badStamp) {
		t.Fatal("regStamp advanced on a failed Load; a later fix landing at this same mtime would never be retried")
	}

	// Restore the overlay at the failed load timestamp; the unchanged cached timestamp must force a retry.
	vaulttest.Put(t, vault.Overlay, "providers", good)
	bumpMtime(t, vault.Path(), badStamp)

	reg3, err := r.registry()
	if err != nil {
		t.Fatalf("third registry() = %v, want the retried Load to succeed", err)
	}
	if reg3 == reg1 {
		t.Error("registry() served the stale cached registry instead of retrying Load at the same mtime")
	}
}

// preFixRegistry reproduces the old unconditional timestamp advance for the regression proof.
func preFixRegistry(r *Router) (*providers.Registry, error) {
	var stamp time.Time
	if fi, err := os.Stat(vault.Path()); err == nil {
		stamp = fi.ModTime()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reg != nil && stamp.Equal(r.regStamp) {
		return r.reg, nil
	}
	reg, err := providers.Load()
	if err != nil {
		if r.reg == nil {
			return nil, err
		}
		reg = r.reg
	}
	r.reg, r.regStamp = reg, stamp // pre-fix bug: stamp advances even when Load failed above
	return reg, nil
}

// The old registry implementation must serve stale data after the same-timestamp overlay repair.
func TestRegistryPreFixLogicCachedTheBadMtime(t *testing.T) {
	vaulttest.Use(t)
	const good = `{"providers":{}}`
	vaulttest.Put(t, vault.Overlay, "providers", good)
	r := newRouter(secret)
	r.log = io.Discard

	reg1, err := preFixRegistry(r)
	if err != nil {
		t.Fatalf("first call = %v", err)
	}
	badStamp := r.regStamp.Add(time.Second)

	vaulttest.Put(t, vault.Overlay, "providers", "not json")
	bumpMtime(t, vault.Path(), badStamp)
	if _, err := preFixRegistry(r); err != nil {
		t.Fatalf("second call = %v", err)
	}
	if !r.regStamp.Equal(badStamp) {
		t.Fatal("replica no longer reproduces the bug: regStamp did not advance on the failed Load")
	}

	vaulttest.Put(t, vault.Overlay, "providers", good)
	bumpMtime(t, vault.Path(), badStamp) // same mtime the failed attempt already claimed as "seen"
	reg3, err := preFixRegistry(r)
	if err != nil {
		t.Fatalf("third call = %v", err)
	}
	if reg3 != reg1 {
		t.Fatal("replica no longer reproduces the bug: it retried Load instead of serving the stale cache")
	}
}
