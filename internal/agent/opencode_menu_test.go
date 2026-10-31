package agent

import (
	"reflect"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

func TestMenuIDCanonical(t *testing.T) {
	for in, want := range map[string]string{
		"databricks-glm-5-2": "glm-5-2", "glm-5-2": "glm-5-2", "databricks-gpt-6-1-sol": "gpt-6-1-sol", "kimi-k3": "kimi-k3",
	} {
		if got := MenuID(in); got != want {
			t.Errorf("MenuID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDefaultMenuLabel(t *testing.T) {
	for in, want := range map[string]string{
		"glm-5-2": "GLM 5.2", "claude-opus-5-5": "Claude Opus 5.5", "gpt-6-1-sol": "GPT 6.1 Sol", "gpt-oss-120b": "GPT OSS 120b",
	} {
		if got := DefaultMenuLabel(in); got != want {
			t.Errorf("DefaultMenuLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMenuAddRemove(t *testing.T) {
	base := []providers.OpencodeModel{{ID: "kimi-k3", Label: "Kimi K3"}}
	got := MenuAdd(base, "databricks-glm-5-2", "", nil)
	if len(base) != 1 || len(got) != 2 || got[1] != (providers.OpencodeModel{ID: "glm-5-2", Label: "GLM 5.2"}) {
		t.Fatalf("add: base=%v got=%v", base, got)
	}
	if g := MenuAdd(nil, "gpt-5-5", "", nil); !g[0].Responses {
		t.Error("gpt- ids must default to Responses")
	}
	no := false
	if g := MenuAdd(nil, "gpt-5-5", "", &no); g[0].Responses {
		t.Error("explicit responses=false must win")
	}
	if g := MenuAdd(got, "glm-5-2", "Mine", nil); len(g) != 2 || g[1].Label != "Mine" {
		t.Errorf("update in place: %v", g)
	}
	out, ok := MenuRemove(got, "databricks-glm-5-2")
	if !ok || len(out) != 1 || out[0].ID != "kimi-k3" || len(got) != 2 {
		t.Errorf("remove: ok=%v out=%v got=%v", ok, out, got)
	}
	if _, ok := MenuRemove(got, "nope"); ok {
		t.Error("removing an absent id must report false")
	}
}

func TestParseMenuImportPlain(t *testing.T) {
	got, err := ParseMenuImport([]byte("# c\nglm-5-2\ndatabricks-kimi-k3,Kimi\n\ngpt-5-5,Five,responses\nglm-5-2,Dup\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []providers.OpencodeModel{{ID: "glm-5-2", Label: "Dup"}, {ID: "kimi-k3", Label: "Kimi"}, {ID: "gpt-5-5", Label: "Five", Responses: true}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

func TestParseMenuImportOpencodeJSON(t *testing.T) {
	doc := `{"provider":{
 "databricks-qa":{"models":{"databricks-kimi-k3":{"name":"Kimi K3 · qa"}}},
 "databricks-qa-gpt":{"models":{"databricks-gpt-5-5":{"name":"GPT 5.5 · qa"}}},
 "openai":{"models":{"x":{"name":"X"}}}}}`
	got, err := ParseMenuImport([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	want := []providers.OpencodeModel{{ID: "gpt-5-5", Label: "GPT 5.5", Responses: true}, {ID: "kimi-k3", Label: "Kimi K3"}}
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			found = found || g == w
		}
		if !found {
			t.Errorf("missing %v in %v", w, got)
		}
	}
	if _, err := ParseMenuImport([]byte(`{"provider":{"openai":{"models":{"x":{}}}}}`)); err == nil {
		t.Error("json without a databricks provider must error")
	}
}

func TestDiffMenu(t *testing.T) {
	old := []providers.OpencodeModel{{ID: "a", Label: "A"}, {ID: "b", Label: "B"}}
	next := []providers.OpencodeModel{{ID: "b", Label: "B2"}, {ID: "c", Label: "C"}}
	got := DiffMenu(old, next)
	if len(got) != 3 || got[0].Op != "+" || got[0].Model.ID != "c" || got[1].Op != "-" || got[1].Model.ID != "a" ||
		got[2].Op != "~" || got[2].Was == nil || got[2].Was.Label != "B" {
		t.Errorf("diff = %+v", got)
	}
	if len(DiffMenu(old, old)) != 0 {
		t.Error("identical menus must diff empty")
	}
}
