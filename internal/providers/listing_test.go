package providers

import (
	"strings"
	"testing"
)

func TestProviderModeLabels(t *testing.T) {
	cases := []struct {
		p    *Provider
		want string
	}{
		{&Provider{Auth: Auth{Type: AuthOAuth}}, "oauth"},
		{&Provider{Preset: PresetDatabricks, Auth: Auth{Type: AuthKey}}, "oauth"},
		{&Provider{Auth: Auth{Type: AuthM2M}}, "m2m"},
		{&Provider{Auth: Auth{Type: AuthCLI}}, "cli"},
		{&Provider{Auth: Auth{Type: AuthNone}}, "-"},
		{&Provider{Auth: Auth{Type: AuthKey, Env: "K"}}, "key"},
		{&Provider{Auth: Auth{Type: AuthPAT}}, "pat"},
	}
	for _, c := range cases {
		if got := providerMode(c.p); got != c.want {
			t.Errorf("providerMode(%+v) = %q, want %q", c.p.Auth, got, c.want)
		}
	}
}

// ModelTableLines aligns every column to the widest cell in it, including the header, and drops a
// gemini "models/" id prefix for display only.
func TestModelTableLinesAligns(t *testing.T) {
	infos := []ModelInfo{
		{ID: "models/gemini-2.5-flash", Name: "Gemini 2.5 Flash", UseCase: "Balanced", Capabilities: ""},
		{ID: "x", Name: "y", UseCase: "High volume", Capabilities: "vision · x2"},
	}
	lines := modelTableLines(infos)
	stdout := strings.Join(lines, "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want a header + 2 rows:\n%s", len(lines), stdout)
	}
	if strings.Contains(lines[1], "models/") {
		t.Errorf("row kept the gemini models/ prefix: %q", lines[1])
	}
	// Every line's "Use case" column must start at the same byte offset.
	idx := strings.Index(lines[0], "Use case")
	for i, l := range lines {
		if got := strings.Index(l, strings.Fields(l)[0]); got != 0 {
			t.Fatalf("line %d not left-aligned: %q", i, l)
		}
	}
	for i := 1; i < len(lines); i++ {
		col := strings.Index(lines[i], infos[i-1].UseCase)
		if col != idx {
			t.Errorf("line %q: Use case column at %d, want %d (header's)", lines[i], col, idx)
		}
	}
}
