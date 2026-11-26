package pi

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
)

func TestParsePackages(t *testing.T) {
	sha := strings.Repeat("a", 40)
	ok := []struct{ in, name, ver string }{
		{"npm:foo@1.2.3", "foo", "1.2.3"},
		{"npm:@scope/foo-bar@0.10.0-beta.1", "@scope/foo-bar", "0.10.0-beta.1"},
		{"git:github.com/o/r@" + sha, "github.com/o/r", sha},
		{"./ext/x", "ext/x", ""},
		{"/opt/ext", "/opt/ext", ""},
	}
	for _, c := range ok {
		got, err := ParsePackages(c.in)
		if err != nil || len(got) != 1 || got[0].Name != c.name || got[0].Version != c.ver || got[0].Spec != c.in {
			t.Errorf("%q = %+v, %v", c.in, got, err)
		}
	}
	if got, err := ParsePackages("npm:a@1.0.0,git:github.com/o/r@" + sha); err != nil || len(got) != 2 {
		t.Errorf("list = %+v, %v", got, err)
	}
}

func TestParsePackagesRejects(t *testing.T) {
	bad := []string{"npm:foo", "npm:foo@^1", "npm:foo@~1.2.3", "npm:foo@>1.0.0", "npm:foo@*", "npm:foo@1.x", "npm:foo@latest",
		"npm:foo@1.2", "npm:@scope@1.0.0", "git:github.com/o/r@main", "git:github.com/o/r@v1.2.3", "git:github.com/o/r@HEAD",
		"git:github.com/o/r@abc123", "git:github.com/o/r", "https://github.com/o/r", "-e", "--approve", "npm:a@1.0.0,",
		"npm:a@1.0.0, npm:b@1.0.0", "npm:a b@1.0.0", "./../x", "/a/../b", "./a;b", "rel/path", "npm:a@1.0.0,npm:a@1.0.1"}
	for _, in := range bad {
		if _, err := ParsePackages(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
	if _, err := ParsePackages("git:github.com/o/r@v1"); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Errorf("tag error = %v", err)
	}
}

func TestApprovedAndExtra(t *testing.T) {
	a, _ := ParsePackages("npm:foo@1.2.3")
	if !Approved("npm:foo@1.2.3", a) || Approved("npm:foo@1.2.4", a) {
		t.Error("approval must be by exact spec")
	}
	if validatePackages("npm:foo@^1") == nil || validatePackages("main") == nil || validatePackages("npm:a@1.0.0") != nil {
		t.Error("validatePackages")
	}
	if got := (Package{Kind: KindLocal, Name: "ext/x"}).LocalPath("/cfg"); got != "/cfg/ext/x" {
		t.Errorf("LocalPath = %q", got)
	}
}

// A looked-up row is a copy: changing its extras declaration must not change the registry's.
func TestLookupSchemaClonesExtras(t *testing.T) {
	s, _ := agent.LookupSchema("pi")
	s.CLI.Extras["injected"] = nil
	s.CLI.ExtraOwnerOnly[0] = "x"
	again, _ := agent.LookupSchema("pi")
	if _, leaked := again.CLI.Extras["injected"]; leaked || again.CLI.Extras == nil || again.CLI.ExtraOwnerOnly[0] != extraPackages {
		t.Errorf("registry row changed through a lookup: %v %v", again.CLI.Extras, again.CLI.ExtraOwnerOnly)
	}
}
