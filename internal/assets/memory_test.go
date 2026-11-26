package assets

import (
	"strings"
	"testing"
	"text/template"
)

func TestMemoryRenderMatchesTextTemplate(t *testing.T) {
	cases := []struct {
		name  string
		input any
	}{
		{"bank/load", ""}, {"bank/load", "pre"},
		{"recall/header", true}, {"recall/header", false},
		{"recall/line", tmplFields{"File": "f.md", "Text": "t", "Label": true}},
		{"recall/line", tmplFields{"File": "f.md", "Text": "t", "Label": false}},
		{"git-activity.tmpl", tmplFields{"Commits": "c1", "Files": ""}},
		{"git-activity.tmpl", tmplFields{"Commits": "", "Files": "a.go"}},
		{"session-tags.tmpl", []string{"DECISION: x", "BUG: y"}}, {"session-tags.tmpl", []string(nil)},
		{"misc/update-recovery", tmplFields{"ID": "s1", "Status": "failed", "Log": "/l"}},
		{"misc/update-recovery", tmplFields{"ID": "s1", "Status": "failed", "Log": ""}},
		{"misc/more-entries", tmplFields{"Count": 3, "Router": "catalog.md"}},
		{"misc/unresolved", 7},
	}
	for _, c := range cases {
		stem, block, folded := strings.Cut(c.name, "/")
		if !folded {
			stem, block = strings.TrimSuffix(c.name, ".tmpl"), c.name
		}
		ref := template.Must(template.New("").Option("missingkey=error").ParseFS(files, "memory/prompts/"+stem+".tmpl"))
		var want strings.Builder
		if err := ref.ExecuteTemplate(&want, block, c.input); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := renderMemory(c.name, c.input); got != want.String() {
			t.Errorf("%s(%v): got %q, want %q", c.name, c.input, got, want.String())
		}
	}
}

func TestMemoryFolderRolesFreshCopy(t *testing.T) {
	roles := MemoryFolderRoles()
	if len(roles) == 0 {
		t.Fatal("folder roles are empty")
	}
	want := roles[0]
	roles[0] = [2]string{"changed", "changed"}
	if got := MemoryFolderRoles()[0]; got != want {
		t.Fatalf("mutating a result changed cached roles: %v", got)
	}
}

func TestMemoryEmptyProcedurePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("empty embedded procedure must fail")
		}
	}()
	memoryProcedure([]byte(" \n\t"))
}
