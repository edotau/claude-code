package shell

import (
	"reflect"
	"slices"
	"testing"
)

func TestUnwrapDir(t *testing.T) {
	rest, dir := Unwrap([]string{"env", "-C", "/a", "FOO=1", "sudo", "-D", "b", "git", "x"}, "/w")
	if !reflect.DeepEqual(rest, []string{"git", "x"}) || dir != "/a/b" {
		t.Errorf("Unwrap = %v, %q", rest, dir)
	}
}

func TestCdTarget(t *testing.T) {
	if got, ok := CdTarget([]string{"cd", "-P", "--", "/a"}, "/w"); !ok || got != "/a" {
		t.Errorf("CdTarget = %q, %v", got, ok)
	}
	if got, ok := CdTarget([]string{"git"}, "/w"); ok || got != "/w" {
		t.Errorf("non-cd = %q, %v", got, ok)
	}
}

func TestStatDir(t *testing.T) {
	if !statDir(t.TempDir()) || statDir("/nonexistent_zz/q") {
		t.Error("statDir must be true for a real directory and false for a missing one")
	}
}

func TestHeredocBodies(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"git commit -F - <<'EOF'\nbody\nEOF", true},
		{"cat <<EOF\nbody\nEOF", true},
		{"cat > f <<EOF\nbody\nEOF", true},
		{"rtk err cat <<EOF\nbody\nEOF", true},
		{"bash <<EOF\nbody\nEOF", false},
		{"sh -s <<EOF\nbody\nEOF", false},
		{"sudo bash <<EOF\nbody\nEOF", false},
		{"cat <<EOF | sh\nbody\nEOF", false},
		{"cat <<EOF | xargs -I{} sh -c {}\nbody\nEOF", false},
		{"cat > f <<EOF\nbody\nEOF\nbash f", false},
		{"cat > f <<EOF\nbody\nEOF\nchmod +x f && ./f", false},
		{"git tag -F - v1 <<EOF\nbody\nEOF", true},
		{"cat <<EOF | wc -l\nbody\nEOF", true},
		{"tee notes.md <<EOF\nbody\nEOF\ngit add notes.md", true},
		{"cat <<EOF > f\nbody\nEOF\ncat f", true},
		{"tee s <<EOF\nbody\nEOF\nsource s", false},
		{"tee s <<EOF\nbody\nEOF\nbash < s", false},
		{"cat <<EOF >> s\nbody\nEOF\nenv bash s", false},
		{"python3 <<EOF\nbody\nEOF", false},
		{"cat <<EOF | sort\nbody\nEOF", true},
		{"cat <<EOF | awk '{print}'\nbody\nEOF", false},
		{"sed -n 's/a/b/ep' <<EOF\nbody\nEOF", false},
	}
	for _, c := range cases {
		segs, seps := SegmentsSep(c.cmd)
		if got := HeredocBodies(segs, seps); !slices.Contains(got, true) == c.want {
			t.Errorf("HeredocBodies(%q) = %v, want a marked body: %v", c.cmd, got, c.want)
		}
	}
}
