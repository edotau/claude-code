package askspec

import (
	"slices"
	"strings"
	"testing"

	_ "github.com/edotau/claude-code/internal/nonet"
)

func testSchema() Schema {
	return Schema{Fields: []Field{
		{Name: "agent", Kind: "string"},
		{Name: "fallback", Kind: "string", Choices: []string{"none", "pin", "agents", "all"}},
		{Name: "model", Kind: "string"},
	}}
}

func TestArgvNearestKey(t *testing.T) {
	s := testSchema()
	argv, prompt, _, err := s.Argv([]byte(`{"prompt":"hi","model":"opus","agent":"codex"}`))
	if want := []string{"--agent", "codex", "--model", "opus"}; err != nil || prompt != "hi" || !slices.Equal(argv, want) {
		t.Fatalf("argv = %q prompt %q err %v, want %q", argv, prompt, err, want)
	}
	if _, _, _, err := s.Argv([]byte(`{"agnet":"codex"}`)); err == nil || !strings.Contains(err.Error(), `"agent"`) {
		t.Fatalf("unknown key err = %v, want one naming agent", err)
	}
	if _, _, _, err := s.Argv([]byte(`{"fallback":"bogus"}`)); err == nil || !strings.Contains(err.Error(), "--fallback") {
		t.Fatalf("bad choice err = %v, want one naming --fallback", err)
	}
}

func TestArgvContractKey(t *testing.T) {
	_, _, task, err := testSchema().Argv([]byte(`{"own":["a.go"],"contract":{"unit":"func x","kind":"function"}}`))
	if err != nil || task.Contract == nil || task.Contract.Unit != "func x" || !task.Set() {
		t.Fatalf("task = %+v err %v", task, err)
	}
	if _, _, _, err := testSchema().Argv([]byte(`{"contract":{"unit":"x","knd":"f"}}`)); err == nil {
		t.Fatal("unknown contract field accepted")
	}
}

func TestArgvTaskKeys(t *testing.T) {
	argv, _, task, err := testSchema().Argv([]byte(`{"own":["a.go"],"report":"r.md","verify":"go vet ./x","model":"m"}`))
	if want := (Task{Own: []string{"a.go"}, Report: "r.md", Verify: "go vet ./x"}); err != nil ||
		!slices.Equal(task.Own, want.Own) || task.Report != want.Report || task.Verify != want.Verify ||
		!slices.Equal(argv, []string{"--model", "m"}) {
		t.Fatalf("task = %+v argv %q err %v", task, argv, err)
	}
	if _, _, _, err := testSchema().Argv([]byte(`{"own":"a.go"}`)); err == nil || !strings.Contains(err.Error(), "own") {
		t.Fatalf("wrong type err = %v, want one naming own", err)
	}
}

func TestArgvActionKey(t *testing.T) {
	_, _, task, err := testSchema().Argv([]byte(`{"action":"diff","own":["a.go"]}`))
	if err != nil || task.Action != "diff" || len(task.Own) != 1 {
		t.Fatalf("task = %+v err %v", task, err)
	}
	if _, _, _, err := testSchema().Argv([]byte(`{"action":"rm"}`)); err == nil || !strings.Contains(err.Error(), "gate|diff") {
		t.Fatalf("unknown action err = %v, want one listing the actions", err)
	}
}

func TestArgvContractRoleParent(t *testing.T) {
	_, _, task, err := testSchema().Argv([]byte(`{"contract":{"unit":"x","kind":"function","role":"fleet","parent":"d-1"}}`))
	if err != nil || task.Contract.Role != "fleet" || task.Contract.Parent != "d-1" {
		t.Fatalf("task = %+v err %v", task.Contract, err)
	}
	for _, bad := range []string{`{"role":"fleet"}`, `{"parent":"d-1"}`, `{"role":"boss","parent":"d-1"}`} {
		doc := `{"contract":{"unit":"x","kind":"function",` + bad[1:] + `}`
		if _, _, _, err := testSchema().Argv([]byte(doc)); err == nil {
			t.Errorf("contract %s accepted", bad)
		}
	}
}
