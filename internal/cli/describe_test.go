package cli

import "testing"

// describeVerbs must carry every listed verb with its spec node, so describe never drifts from the table.
func TestDescribeVerbsCoverTable(t *testing.T) {
	got := map[string]bool{}
	for _, v := range describeVerbs() {
		got[v.Name] = true
	}
	for _, c := range commands() {
		if !got[c.name] {
			t.Errorf("describeVerbs lacks table verb %q", c.name)
		}
	}
	for _, want := range []string{"agents", "doctor", "configure", "memory"} {
		if !got[want] {
			t.Errorf("describeVerbs lacks verb %q", want)
		}
	}
}
