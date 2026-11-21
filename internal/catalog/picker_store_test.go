package catalog

import (
	"reflect"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestPickerNewSkipsDatabricksLlama3Instruct(t *testing.T) {
	p := &providers.Provider{Name: "dev", Provider: providers.GroupDatabricks, Preset: providers.PresetDatabricks}
	ready := func(id string) providers.ModelInfo {
		return providers.ModelInfo{ID: id, Ready: "READY", Task: "llm/v1/chat"}
	}
	live := []providers.ModelInfo{ready("databricks-meta-llama-3-1-8b-instruct"), ready("databricks-meta-llama-3-3-70b-instruct"),
		ready("databricks-new-model")}
	got := pickerNewWithBaseline(p, nil, live, nil, nil)
	if want := []providers.ModelInfo{{ID: "databricks-new-model"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("additions = %+v, want %+v", got, want)
	}
}

func dropFixture(t *testing.T) *providers.Provider {
	t.Helper()
	vaulttest.UseKeyring(t)
	p := &providers.Provider{Name: "dev", Provider: providers.GroupDatabricks, Preset: providers.PresetDatabricks}
	if err := providers.SetOpencodeMenu([]providers.OpencodeModel{{ID: "kimi-k3", Label: "K"}, {ID: "gpt-5", Label: "G5"}, {ID: "gpt-5-mini", Label: "G5m"}, {ID: "glm-5-3", Label: "Z"}}); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustAppend(t *testing.T, p *providers.Provider, live []providers.ModelInfo, want string) {
	t.Helper()
	added, err := AppendPickerModels(p, live)
	if err != nil || len(added) != 1 || added[0].ID != want {
		t.Fatalf("apply added %v, %v; want only %s", added, err, want)
	}
}

func TestDropBlocksApplyUntilUndrop(t *testing.T) {
	p := dropFixture(t)
	live := []providers.ModelInfo{{ID: "databricks-gpt-5", Ready: "READY", Task: "llm/v1/chat"}, {ID: "databricks-new", Ready: "READY", Task: "llm/v1/chat"}}
	if _, err := providers.DropFromMenu([]string{"gpt-5"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := EditPickerDrops(p, []string{"gpt-5"}, false, true); err != nil {
		t.Fatal(err)
	}
	mustAppend(t, p, live, "databricks-new")
	if skipped, _ := DroppedLive(p, live); !reflect.DeepEqual(skipped, []string{"databricks-gpt-5"}) {
		t.Fatalf("skipped = %v", skipped)
	}
	if _, err := EditPickerDrops(p, []string{"gpt-5"}, true, true); err != nil {
		t.Fatal(err)
	}
	mustAppend(t, p, live, "databricks-gpt-5")
}

func TestDropRemovesPickerAdditionAndMenuFor(t *testing.T) {
	p := dropFixture(t)
	if _, err := AppendPickerModels(p, []providers.ModelInfo{{ID: "databricks-extra", Ready: "READY", Task: "llm/v1/chat"}}); err != nil {
		t.Fatal(err)
	}
	preview, err := EditPickerDrops(p, []string{"extra", "gpt-5"}, false, false)
	if err != nil || !reflect.DeepEqual(preview, []string{"databricks-extra"}) {
		t.Fatalf("preview = %v, %v", preview, err)
	}
	if saved, _ := PickerModels(p); len(saved) != 1 {
		t.Fatalf("preview changed saved additions: %v", saved)
	}
	if _, err := EditPickerDrops(p, []string{"extra", "gpt-5"}, false, true); err != nil {
		t.Fatal(err)
	}
	menu, err := OpencodeMenuFor(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range menu {
		if m.ID == "databricks-extra" || m.ID == "gpt-5" {
			t.Errorf("dropped id %s still in the picker menu", m.ID)
		}
	}
}

func TestCostOptionsDoNotChangeThePickerScope(t *testing.T) {
	p := &providers.Provider{Name: "dev", Host: "https://example.invalid"}
	before := pickerScope(p)
	p.Options.WarehouseID, p.Options.Prices = "wh1", map[string]providers.Price{"m": {Input: 1}}
	if after := pickerScope(p); after != before {
		t.Errorf("setting cost options changed the picker scope: %s -> %s", before, after)
	}
}
