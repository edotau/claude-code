package assets

import (
	"strings"
	"testing"
	"text/template"
)

func TestMemoryRenderMatchesTextTemplate(t *testing.T) {
	ref := template.Must(template.New("").Option("missingkey=error").ParseFS(files, "files/memory/prompts/*.tmpl"))
	cases := []struct {
		name  string
		input any
	}{
		{"bank-load.tmpl", ""}, {"bank-load.tmpl", "pre"},
		{"recall-header.tmpl", true}, {"recall-header.tmpl", false},
		{"recall-line.tmpl", tmplFields{"File": "f.md", "Text": "t", "Label": true}},
		{"recall-line.tmpl", tmplFields{"File": "f.md", "Text": "t", "Label": false}},
		{"git-activity.tmpl", tmplFields{"Commits": "c1", "Files": ""}},
		{"git-activity.tmpl", tmplFields{"Commits": "", "Files": "a.go"}},
		{"session-tags.tmpl", []string{"DECISION: x", "BUG: y"}}, {"session-tags.tmpl", []string(nil)},
		{"update-recovery.tmpl", tmplFields{"ID": "s1", "Status": "failed", "Log": "/l"}},
		{"update-recovery.tmpl", tmplFields{"ID": "s1", "Status": "failed", "Log": ""}},
		{"more-entries.tmpl", tmplFields{"Count": 3, "Router": "catalog.md"}},
		{"unresolved.tmpl", 7},
	}
	for _, c := range cases {
		var want strings.Builder
		if err := ref.ExecuteTemplate(&want, c.name, c.input); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := renderMemory(c.name, c.input); got != want.String() {
			t.Errorf("%s(%v): got %q, want %q", c.name, c.input, got, want.String())
		}
	}
}

func TestMemoryFramingSentinels(t *testing.T) {
	if got := MemoryRecallFooter(); got != "\nLoad one with: claude-code memory show <id>\n</memory-recall>" {
		t.Fatalf("recall footer changed: %q", got)
	}
	if got := MemoryRecallMarker(); got != "<memory-recall>" {
		t.Fatalf("recall marker changed: %q", got)
	}
	if got := MemoryRecallLine("", "body", true); got != "- [] body" {
		t.Fatalf("empty filename framing changed: %q", got)
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
