package plans

import "testing"

// A longer fence is closed only by an equal-or-longer fence of the same character, so inner examples stay fenced.
func TestFenceNestingHidesSampleCheckboxes(t *testing.T) {
	md := "- [ ] 1.1 Real owns: a.go\n\n````md\n```\n- [ ] 9.9 Sample owns: x.go\n```\n- [ ] 9.8 Sample2 owns: y.go\n````\n\n~~~\n```\n- [ ] 9.7 Tilde owns: z.go\n~~~\n\n- [ ] 2.1 Second owns: b.go\n"
	p := ParseBytes("p.md", []byte(md))
	if len(p.Tasks) != 2 || p.Tasks[0].ID != "1.1" || p.Tasks[1].ID != "2.1" {
		t.Fatalf("tasks = %+v; want only 1.1 and 2.1", p.Tasks)
	}
}
