package opencode

import (
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

func TestDefaultMenuLabel(t *testing.T) {
	for in, want := range map[string]string{
		"glm-5-2": "GLM 5.2", "claude-opus-5-5": "Claude Opus 5.5", "gpt-6-1-sol": "GPT 6.1 Sol", "gpt-oss-120b": "GPT OSS 120b",
	} {
		if got := defaultMenuLabel(in); got != want {
			t.Errorf("defaultMenuLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMenuAddRemove(t *testing.T) {
	base := []providers.OpencodeModel{{ID: "kimi-k3", Label: "Kimi K3"}}
	got := menuAdd(base, "databricks-glm-5-2", "", nil)
	t.Run("add", func(t *testing.T) { testMenuAdd(t, base, got) })
	t.Run("responses default", testMenuResponsesDefault)
	t.Run("responses override", testMenuResponsesOverride)
	t.Run("update", func(t *testing.T) { testMenuUpdate(t, got) })
	t.Run("remove", func(t *testing.T) { testMenuRemove(t, got) })
	t.Run("absent", func(t *testing.T) { testMenuAbsent(t, got) })
}

func testMenuAdd(t *testing.T, base, got []providers.OpencodeModel) {
	t.Helper()
	if len(base) != 1 || len(got) != 2 || got[1] != (providers.OpencodeModel{ID: "glm-5-2", Label: "GLM 5.2"}) {
		t.Fatalf("add: base=%v got=%v", base, got)
	}
}

func testMenuResponsesDefault(t *testing.T) {
	t.Helper()
	if g := menuAdd(nil, "gpt-5-5", "", nil); !g[0].Responses {
		t.Error("gpt- ids must default to Responses")
	}
}

func testMenuResponsesOverride(t *testing.T) {
	t.Helper()
	no := false
	if g := menuAdd(nil, "gpt-5-5", "", &no); g[0].Responses {
		t.Error("explicit responses=false must win")
	}
}

func testMenuUpdate(t *testing.T, got []providers.OpencodeModel) {
	t.Helper()
	if g := menuAdd(got, "glm-5-2", "Mine", nil); len(g) != 2 || g[1].Label != "Mine" {
		t.Errorf("update in place: %v", g)
	}
}

func testMenuRemove(t *testing.T, got []providers.OpencodeModel) {
	t.Helper()
	out, ok := MenuRemove(got, "databricks-glm-5-2")
	if !ok || len(out) != 1 || out[0].ID != "kimi-k3" || len(got) != 2 {
		t.Errorf("remove: ok=%v out=%v got=%v", ok, out, got)
	}
}

func testMenuAbsent(t *testing.T, got []providers.OpencodeModel) {
	t.Helper()
	if _, ok := MenuRemove(got, "nope"); ok {
		t.Error("removing an absent id must report false")
	}
}

func TestParseMenuImportPlain(t *testing.T) {
	got, err := parseMenuImport([]byte("# c\nglm-5-2\ndatabricks-kimi-k3,Kimi\n\ngpt-5-5,Five,responses\nglm-5-2,Dup\n"))
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
	got, err := parseMenuImport([]byte(doc))
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
	if _, err := parseMenuImport([]byte(`{"provider":{"openai":{"models":{"x":{}}}}}`)); err == nil {
		t.Error("json without a databricks provider must error")
	}
}

func TestDiffMenu(t *testing.T) {
	old := []providers.OpencodeModel{{ID: "a", Label: "A"}, {ID: "b", Label: "B"}}
	next := []providers.OpencodeModel{{ID: "b", Label: "B2"}, {ID: "c", Label: "C"}}
	got := diffMenu(old, next)
	if len(got) != 3 || got[0].Op != "+" || got[0].Model.ID != "c" || got[1].Op != "-" || got[1].Model.ID != "a" ||
		got[2].Op != "~" || got[2].Was == nil || got[2].Was.Label != "B" {
		t.Errorf("diff = %+v", got)
	}
	if len(diffMenu(old, old)) != 0 {
		t.Error("identical menus must diff empty")
	}
}

func TestParseMenuImportProviderRouting(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, tc := range []struct {
		name      string
		doc       string
		responses bool
	}{
		{"responses npm without suffix", `{"provider":{"databricks":{"npm":"@ai-sdk/openai","models":{"databricks-gpt-6-1-sol":{"name":"GPT 6.1 Sol"}}}}}`, true},
		{"legacy gpt suffix", `{"provider":{"databricks-gpt":{"models":{"databricks-gpt-6-1-sol":{"name":"GPT 6.1 Sol"}}}}}`, true},
		{"chat npm overrides suffix", `{"provider":{"databricks-gpt":{"npm":"@ai-sdk/openai-compatible","models":{"databricks-gpt-6-1-sol":{"name":"GPT 6.1 Sol"}}}}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseMenuImport([]byte(tc.doc))
			if err != nil {
				t.Fatal(err)
			}
			want := []providers.OpencodeModel{{ID: "gpt-6-1-sol", Label: "GPT 6.1 Sol", Responses: tc.responses}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("provider routing: got %+v, want %+v", got, want)
			}
		})
	}
}

func TestParseMenuImportLongLine(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	label := strings.Repeat("long-label", 8<<10)
	got, err := parseMenuImport([]byte("databricks-kimi-k3," + label + ",responses\n"))
	if err != nil {
		t.Fatalf("valid menu line over 64 KiB: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("parsed %d models, want 1", len(got))
	}
	if got[0].ID != "kimi-k3" || got[0].Label != label || !got[0].Responses {
		t.Fatalf("parsed ID=%q, label matches=%t, responses=%t", got[0].ID, got[0].Label == label, got[0].Responses)
	}
}

func TestParseMenuImportHyphenatedProfile(t *testing.T) {
	want := []providers.OpencodeModel{{ID: "glm-5-2", Label: "GLM 5.2", Responses: true}}
	p := regressionDatabricksProfile(t, "qa-prod", want)
	body := regressionGeneratedMenu(t, p, "glm-5-2")
	if !strings.Contains(string(body), `"harness-qa-prod-gpt"`) {
		t.Fatalf("fixture lacks hyphenated provider ID: %s", body)
	}
	got, err := parseMenuImport(body)
	if err != nil {
		t.Fatalf("import generated hyphenated profile: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("imported menu = %+v, want %+v", got, want)
	}
}

func TestParseMenuImportPreservesExplicitRouting(t *testing.T) {
	menu := []providers.OpencodeModel{
		{ID: "glm-5-2", Label: "GLM 5.2", Responses: true},
		{ID: "gpt-6-1-sol", Label: "GPT 6.1 Sol", Responses: false},
	}
	p := regressionDatabricksProfile(t, "databricks-test", menu)
	got, err := parseMenuImport(regressionGeneratedMenu(t, p, "glm-5-2"))
	if err != nil {
		t.Fatal(err)
	}
	want := []providers.OpencodeModel{menu[1], menu[0]}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("import must retain explicit Responses and chat overrides: got %+v, want %+v", got, want)
	}
}
