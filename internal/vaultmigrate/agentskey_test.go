package vaultmigrate

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestAgentsOverlayRename(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "vendors", `{"vendors":{"codex":{"launch":{"args":["x"]}}}}`)
	reg, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	p, err := Compute(reg)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range p.Changes {
		found = found || c.Old == "overlay/vendors" && c.New == "overlay/agents"
	}
	if !found {
		t.Fatalf("plan lacks the vendors -> agents step: %+v", p.Changes)
	}
	if _, err := Apply(reg); err != nil {
		t.Fatal(err)
	}
	_ = vault.View(func(tx *vault.Tx) error {
		got, _ := tx.Get(vault.Overlay, "agents")
		if !strings.Contains(got, `"agents":{"codex"`) || strings.Contains(got, `"vendors"`) {
			t.Errorf("overlay/agents = %q", got)
		}
		if old, _ := tx.Get(vault.Overlay, "vendors"); old != "" {
			t.Errorf("overlay/vendors left behind: %q", old)
		}
		return nil
	})
	if p2, _ := Compute(reg); p2 != nil {
		for _, c := range p2.Changes {
			if c.Old == "overlay/vendors" {
				t.Errorf("not idempotent: %+v", c)
			}
		}
	}
}

func hasStep(p *Plan, old, nw string) bool {
	for _, c := range p.Changes {
		if c.Old == old && c.New == nw {
			return true
		}
	}
	return false
}

func TestAgentsOverlayDryRunThenRerun(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "vendors", `{"vendors":{"codex":{}}}`)
	reg, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	p, err := Compute(reg)
	if err != nil || !hasStep(p, "overlay/vendors", "overlay/agents") {
		t.Fatalf("plan: %+v %v", p, err)
	}
	_ = vault.View(func(tx *vault.Tx) error {
		if got, _ := tx.Get(vault.Overlay, "vendors"); got == "" {
			t.Error("Compute (dry run) mutated the vault")
		}
		if got, _ := tx.Get(vault.Overlay, "agents"); got != "" {
			t.Errorf("Compute wrote agents: %q", got)
		}
		return nil
	})
	for i := 0; i < 2; i++ { // second Apply is a no-op
		if _, err := Apply(reg); err != nil {
			t.Fatalf("Apply #%d: %v", i+1, err)
		}
	}
	_ = vault.View(func(tx *vault.Tx) error {
		if got, _ := tx.Get(vault.Overlay, "agents"); !strings.Contains(got, `"agents":{"codex"`) {
			t.Errorf("agents after re-run = %q", got)
		}
		return nil
	})
	if p2, _ := Compute(reg); p2 != nil && hasStep(p2, "overlay/vendors", "overlay/agents") {
		t.Error("step re-planned after apply")
	}
}

func TestAgentsOverlayUntouchedWithoutVendors(t *testing.T) {
	vaulttest.Use(t)
	const body = `{"agents":{"codex":{"launch":{"args":["y"]}}}}`
	vaulttest.Put(t, vault.Overlay, "agents", body)
	reg, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := Compute(reg); p != nil && hasStep(p, "overlay/vendors", "overlay/agents") {
		t.Error("planned a rename with no vendors overlay")
	}
	if _, err := Apply(reg); err != nil {
		t.Fatal(err)
	}
	_ = vault.View(func(tx *vault.Tx) error {
		if got, _ := tx.Get(vault.Overlay, "agents"); got != body {
			t.Errorf("agents overlay changed: %q", got)
		}
		return nil
	})
}

func TestAgentsOverlayExistingAgentsWins(t *testing.T) {
	vaulttest.Use(t)
	const keep = `{"agents":{"codex":{"launch":{"args":["keep"]}}}}`
	vaulttest.Put(t, vault.Overlay, "agents", keep)
	vaulttest.Put(t, vault.Overlay, "vendors", `{"vendors":{"gemini":{}}}`)
	reg, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = Apply(reg)
	_ = vault.View(func(tx *vault.Tx) error {
		if got, _ := tx.Get(vault.Overlay, "agents"); got != keep {
			t.Errorf("existing overlay/agents overwritten: %q", got)
		}
		return nil
	})
}
