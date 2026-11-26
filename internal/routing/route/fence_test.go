package route

import (
	"reflect"
	"testing"

	_ "github.com/edotau/claude-code/internal/platform/nonet"
)

func TestFence(t *testing.T) {
	closed := func(p ...string) Fence { return Fence{Profiles: p} }
	open := Fence{Open: true}
	cases := []struct {
		name          string
		parent, child Fence
		want          Fence
	}{
		{"open/open", open, open, open},
		{"open parent, closed child", open, closed("qa", "dev"), closed("dev", "qa")},
		{"closed/closed overlap", closed("dev", "qa"), closed("qa", "prod"), closed("qa")},
		{"closed/closed disjoint", closed("dev"), closed("prod"), closed()},
		{"closed parent, open child", closed("dev"), open, closed("dev")},
	}
	for _, c := range cases {
		got := c.parent.Narrow(c.child)
		if got.Open != c.want.Open || !reflect.DeepEqual(append([]string{}, got.Profiles...), append([]string{}, c.want.Profiles...)) {
			t.Errorf("%s: got %+v want %+v", c.name, got, c.want)
		}
	}
	if !open.Allows("x") || !closed("a").Allows("a") || closed("a").Allows("b") {
		t.Fatal("Allows wrong")
	}
}
