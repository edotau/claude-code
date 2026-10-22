package memory

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestEnsureHomeBankRenamedMovesGlobalToHome: a store whose unified-store cutover completed before the
// global/->home/ rename landed gets its leftover global/ dir renamed to home/, contents intact.
func TestEnsureHomeBankRenamedMovesGlobalToHome(t *testing.T) {
	migrateConfig(t)
	cutover(t)
	global := filepath.Join(StoreDir(), "global")
	write(t, filepath.Join(global, "conventions.md"), "# C\n\n- rule one\n")

	if got := HomeBankDir(); got != filepath.Join(StoreDir(), "home") {
		t.Fatalf("HomeBankDir() = %s, want store/home", got)
	}
	data, err := os.ReadFile(filepath.Join(StoreDir(), "home", "conventions.md"))
	if err != nil || string(data) != "# C\n\n- rule one\n" {
		t.Fatalf("home/conventions.md = %q, %v; contents lost in the rename", data, err)
	}
	if _, err := os.Stat(global); !os.IsNotExist(err) {
		t.Errorf("global/ still present after the rename: %v", err)
	}
}

// TestEnsureHomeBankRenamedSecondRunNoOp: once home/ exists, a later call (fresh resolver, same store)
// leaves it untouched and never recreates global/.
func TestEnsureHomeBankRenamedSecondRunNoOp(t *testing.T) {
	migrateConfig(t)
	cutover(t)
	global := filepath.Join(StoreDir(), "global")
	write(t, filepath.Join(global, "conventions.md"), "# C\n\n- rule one\n")

	_ = HomeBankDir() // first call: triggers the rename

	// A later write straight into home/ must survive an unrelated second resolution.
	write(t, filepath.Join(StoreDir(), "home", "decisionLog.md"), "# D\n")
	if got := HomeBankDir(); got != filepath.Join(StoreDir(), "home") {
		t.Fatalf("HomeBankDir() = %s on second call, want store/home", got)
	}
	if _, err := os.Stat(global); !os.IsNotExist(err) {
		t.Errorf("global/ reappeared on a later resolution: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(StoreDir(), "home", "decisionLog.md")); err != nil || string(data) != "# D\n" {
		t.Errorf("decisionLog.md written after the rename was lost: %v %q", err, data)
	}
}

// TestEnsureHomeBankRenamedBothPresentLeftAlone: both global/ and home/ already exist (e.g. a half-done
// hand migration) — the rename must not touch either, and Doctor must report the leftover for a human.
func TestEnsureHomeBankRenamedBothPresentLeftAlone(t *testing.T) {
	migrateConfig(t)
	cutover(t)
	global := filepath.Join(StoreDir(), "global")
	home := filepath.Join(StoreDir(), "home")
	write(t, filepath.Join(global, "conventions.md"), "# C\n\n- global copy\n")
	write(t, filepath.Join(home, "conventions.md"), "# C\n\n- home copy\n")

	_ = HomeBankDir()

	if data, err := os.ReadFile(filepath.Join(global, "conventions.md")); err != nil || string(data) != "# C\n\n- global copy\n" {
		t.Errorf("global/ was touched when both dirs pre-existed: %v %q", err, data)
	}
	if data, err := os.ReadFile(filepath.Join(home, "conventions.md")); err != nil || string(data) != "# C\n\n- home copy\n" {
		t.Errorf("home/ was touched when both dirs pre-existed: %v %q", err, data)
	}
	problems := Doctor(StoreDir(), DefaultStaleDays, time.Now())
	found := false
	for _, p := range problems {
		if p == "global/ left over; merge into home/ by hand" {
			found = true
		}
	}
	if !found {
		t.Errorf("Doctor did not report the leftover global/: %v", problems)
	}
}
