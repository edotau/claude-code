package agent

import (
	"context"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/memstore"
)

// Given a launching workspace in the env, Then every child env carries it exactly once, explicit Base included.
func TestEnvironPassesMemoryOrigin(t *testing.T) {
	t.Setenv(memstore.OriginEnv, "/work/space")
	want := memstore.OriginEnv + "=/work/space"
	for _, s := range []EnvSpec{{Mode: EnvClaude}, {Mode: EnvAgent}, {Mode: EnvClaude, Base: []string{"PATH=/bin"}}} {
		env := s.Environ(context.Background())
		if n := len(slices.DeleteFunc(slices.Clone(env), func(e string) bool { return e != want })); n != 1 {
			t.Errorf("%+v: %s present %d times, want 1", s, want, n)
		}
	}
}

// Given an outer dispatch's policy in the env, Then a nested policy keeps its Deny rules and plan mode, never fewer.
func TestInheritPermissionsKeepsOuterDeny(t *testing.T) {
	t.Setenv(DispatchPermissionsEnv, `{"mode":"plan","deny":[{"Tool":"Bash","Arg":"rm *"}]}`)
	inner, err := ParsePermissions("auto", []string{"Bash"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := inheritPermissions(inner)
	if err != nil || got.Mode != "plan" || got.Decide("Bash", "rm x") != Deny || got.Decide("Bash", "ls") != Allow {
		t.Fatalf("inherit = %+v, %v; want plan mode, rm denied, ls allowed", got, err)
	}
	if got, _ := inheritPermissions(Permissions{}); len(got.Deny) != 1 {
		t.Fatalf("no inner policy: got %+v, want the outer policy whole", got)
	}
	t.Setenv(DispatchPermissionsEnv, "{bad")
	if _, err := inheritPermissions(inner); err == nil {
		t.Fatal("malformed outer policy: want an error (fail closed)")
	}
}
