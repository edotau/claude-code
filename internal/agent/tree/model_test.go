package tree

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
)

func modelRecord(id, parent, name string, carry bool) agent.Record {
	return agent.Record{
		ID: id, Parent: parent, Agent: name, Status: "ok",
		Repo: "/tmp/tree-model-fixture", Request: agent.RunOptions{Carry: carry},
	}
}

func modelDispatches(records ...agent.Record) []Dispatch {
	var dispatches []Dispatch
	for _, rec := range records {
		dispatches = append(dispatches, dispatchFromRecord(rec, "", time.Unix(0, 0)))
	}
	return dispatches
}

func TestBuild(t *testing.T) {
	for _, tt := range modelCases() {
		t.Run(tt.name, func(t *testing.T) {
			records := modelDispatches(tt.records...)
			before := slices.Clone(records)
			nodes := Build(nil, records)
			got, err := json.Marshal(nodes)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("Build() = %s, want %s", got, tt.want)
			}
			checkModelPayloads(t, nodes)
			if !reflect.DeepEqual(records, before) {
				t.Fatal("Build modified its input")
			}
		})
	}
}

type modelCase struct {
	name    string
	records []agent.Record
	want    string
}

func modelCases() []modelCase {
	return slices.Concat(modelRunCases(), modelCarryCases(), modelParentCases())
}

func modelRunCases() []modelCase {
	return []modelCase{
		{name: "empty", want: `null`},
		{
			name: "plain run", records: []agent.Record{modelRecord("a", "", "codex", false)},
			want: `[{"kind":"dispatch","id":"a","depth":0,"agent":"codex"}]`,
		},
		{
			name: "nested run", records: []agent.Record{
				modelRecord("b", "a", "pi", false), modelRecord("a", "", "codex", false),
			},
			want: `[{"kind":"dispatch","id":"a","depth":0,"agent":"codex","children":[` +
				`{"kind":"dispatch","id":"b","depth":1,"agent":"pi"}]}]`,
		},
	}
}

func modelCarryCases() []modelCase {
	takeover := modelRecord("c", "a", "pi", false)
	takeover.TakeoverOf = "b"
	return []modelCase{
		{
			name: "carrier awaiting child", records: []agent.Record{modelRecord("a", "", "codex", true)},
			want: `[{"kind":"dispatch","id":"a","depth":0,"agent":"codex"}]`,
		},
		{
			name: "carrier with child", records: []agent.Record{
				modelRecord("a", "", "codex", true), modelRecord("b", "a", "codex", false),
			},
			want: `[{"kind":"dispatch","id":"b","depth":0,"via":"a","agent":"codex"}]`,
		},
		{
			name: "carrier with takeover", records: []agent.Record{
				takeover, modelRecord("b", "a", "codex", false), modelRecord("a", "", "codex", true),
				modelRecord("d", "b", "claude", false), modelRecord("e", "c", "pi", false),
			},
			want: `[{"kind":"dispatch","id":"c","depth":0,"via":"a","agent":"codex→pi","children":[` +
				`{"kind":"dispatch","id":"e","depth":1,"agent":"pi"},` +
				`{"kind":"dispatch","id":"d","depth":1,"agent":"claude"}]}]`,
		},
		{
			name: "carrier nested two deep", records: []agent.Record{
				modelRecord("a", "", "codex", true), modelRecord("b", "a", "codex", false),
				modelRecord("c", "b", "pi", true), modelRecord("d", "c", "pi", false),
			},
			want: `[{"kind":"dispatch","id":"b","depth":0,"via":"a","agent":"codex","children":[` +
				`{"kind":"dispatch","id":"d","depth":1,"via":"c","agent":"pi"}]}]`,
		},
		{
			name: "consecutive carriers", records: []agent.Record{
				modelRecord("a", "", "codex", true), modelRecord("b", "a", "codex", true),
				modelRecord("c", "b", "pi", false),
			},
			want: `[{"kind":"dispatch","id":"c","depth":0,"via":"a","agent":"codex→pi"}]`,
		},
	}
}

func modelParentCases() []modelCase {
	return []modelCase{
		{
			name: "orphan", records: []agent.Record{modelRecord("a", "missing", "codex", false)},
			want: `[{"kind":"dispatch","id":"a","depth":0,"agent":"codex"}]`,
		},
		{
			name: "self parent", records: []agent.Record{modelRecord("a", "a", "codex", true)},
			want: `[{"kind":"dispatch","id":"a","depth":0,"agent":"codex"}]`,
		},
		{
			name: "parent cycle", records: []agent.Record{
				modelRecord("a", "b", "codex", true), modelRecord("b", "c", "pi", false),
				modelRecord("c", "a", "claude", false),
			},
			want: `[{"kind":"dispatch","id":"a","depth":0,"agent":"codex"},` +
				`{"kind":"dispatch","id":"b","depth":0,"agent":"pi"},` +
				`{"kind":"dispatch","id":"c","depth":0,"agent":"claude"}]`,
		},
	}
}

func checkModelPayloads(t *testing.T, nodes []Node) {
	t.Helper()
	for _, n := range nodes {
		if n.Dispatch == nil || n.Session != nil || n.Subagent != nil {
			t.Fatalf("dispatch node has invalid payloads: %+v", n)
		}
		id := n.ID
		if n.Via != "" {
			id = n.Via
		}
		if n.Dispatch.ID != id || n.Dispatch.Agent != n.Agent {
			t.Fatalf("dispatch payload disagrees with node: %+v", n)
		}
		checkModelPayloads(t, n.Children)
	}
}

func TestBuildSession(t *testing.T) {
	dispatches := modelDispatches(
		modelRecord("a", "", "codex", true), modelRecord("b", "a", "pi", false),
		modelRecord("c", "b", "claude", false),
	)
	sessions := []Session{{ID: "session", Status: statusActive, Subagents: []Subagent{
		{ID: "sub", Type: "claude", Depth: 9, Dispatches: dispatches},
	}}}
	roots := modelDispatches(modelRecord("root", "", "pi", false))
	nodes := Build(sessions, roots)
	got, err := json.Marshal(nodes)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"kind":"session","id":"session","depth":0,"children":[` +
		`{"kind":"subagent","id":"sub","depth":1,"agent":"claude","children":[` +
		`{"kind":"dispatch","id":"b","depth":2,"via":"a","agent":"codex→pi","children":[` +
		`{"kind":"dispatch","id":"c","depth":3,"agent":"claude"}]}]}]},` +
		`{"kind":"dispatch","id":"root","depth":0,"agent":"pi"}]`
	if string(got) != want {
		t.Fatalf("Build() = %s, want %s", got, want)
	}
	session, sub := nodes[0], nodes[0].Children[0]
	session.Children, sub.Children = nil, nil
	if !reflect.DeepEqual(session, Node{Kind: KindSession, ID: "session", Session: &sessions[0]}) {
		t.Fatalf("invalid session payload: %+v", session)
	}
	wantSub := Node{Kind: KindSubagent, ID: "sub", Depth: 1, Agent: "claude", Subagent: &sessions[0].Subagents[0]}
	if !reflect.DeepEqual(sub, wantSub) {
		t.Fatalf("invalid subagent payload: %+v", sub)
	}
	checkModelPayloads(t, nodes[0].Children[0].Children)
	session.Session.ID, sub.Subagent.ID = "changed", "changed"
	if sessions[0].ID != "session" || sessions[0].Subagents[0].ID != "sub" {
		t.Fatal("node payloads alias input values")
	}
}

func TestBuildFoldPayloadImmutable(t *testing.T) {
	records := modelDispatches(
		modelRecord("a", "", "codex", true), modelRecord("b", "a", "pi", false),
	)
	files := []string{"carrier.go", "untouched.go"}
	records[0].Files = files[:1]
	records[0].Model, records[0].Task = "carrier-model", "carrier task"
	records[1].Files = []string{"worker.go"}
	records[1].Model, records[1].Provider = "worker-model", "fixture"
	records[1].Usage = &wire.Usage{InputTokens: 23, OutputTokens: 7}
	before := slices.Clone(records)
	before[0].Files = slices.Clone(records[0].Files)
	before[1].Files = slices.Clone(records[1].Files)
	usage := *records[1].Usage
	before[1].Usage = &usage
	want := foldInto(before[0], before[1])
	nodes := Build(nil, records)
	if len(nodes) != 1 || !reflect.DeepEqual(nodes[0].Dispatch, &want) {
		t.Fatalf("folded payload = %+v, want %+v", nodes, want)
	}
	if !reflect.DeepEqual(records, before) || files[1] != "untouched.go" {
		t.Fatal("Build modified input values or their backing storage")
	}
}

func TestBuildPrintedOrder(t *testing.T) {
	sessions := []Session{
		{ID: "idle-a", Cwd: "/a"},
		{ID: "active-b", Cwd: "/b", Status: statusActive},
		{ID: "active-a", Cwd: "/a", Status: statusActive},
		{ID: "active-b2", Cwd: "/b", Status: statusActive},
		{ID: "idle-b", Cwd: "/b"},
		{ID: "idle-a2", Cwd: "/a"},
	}
	tree := Tree{Sessions: sessions, Dispatches: []Dispatch{{ID: "job"}}, Free: []string{"a<b.go"}}
	var ids []string
	for _, n := range Build(tree.Sessions, tree.Dispatches) {
		ids = append(ids, n.ID)
	}
	want := []string{"active-b", "active-b2", "active-a", "job", "idle-a", "idle-a2", "idle-b"}
	if !slices.Equal(ids, want) {
		t.Fatalf("root IDs = %v, want %v", ids, want)
	}
	got, err := json.Marshal(tree)
	if err != nil {
		t.Fatal(err)
	}
	type plain Tree
	base, err := json.Marshal(plain(tree))
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := json.Marshal(Build(tree.Sessions, tree.Dispatches))
	if err != nil {
		t.Fatal(err)
	}
	if want := string(base[:len(base)-1]) + `,"nodes":` + string(nodes) + `}`; string(got) != want {
		t.Fatalf("JSON = %s, want %s", got, want)
	}
}

func TestBuildWorkflow(t *testing.T) {
	run := WorkflowRun{ID: "run", Name: "review", Phases: []WorkflowPhase{
		{Name: "first", Agents: []Subagent{{ID: "one", Type: "claude", Dispatches: []Dispatch{
			{ID: "carrier", Agent: "codex", carry: true},
			{ID: "worker", Parent: "carrier", Agent: "pi"},
		}}}},
		{Name: "second", Agents: []Subagent{{ID: "two", Type: "codex"}}},
	}}
	nodes := Build([]Session{{ID: "session", Workflows: []WorkflowRun{run}}}, nil)
	got, err := json.Marshal(nodes)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"kind":"session","id":"session","depth":0,"children":[` +
		`{"kind":"workflow","id":"run","depth":1,"agent":"review","children":[` +
		`{"kind":"subagent","id":"one","depth":2,"agent":"claude","children":[` +
		`{"kind":"dispatch","id":"worker","depth":3,"via":"carrier","agent":"codex→pi"}]},` +
		`{"kind":"subagent","id":"two","depth":2,"agent":"codex"}]}]}]`
	if string(got) != want {
		t.Fatalf("Build() = %s, want %s", got, want)
	}
	if wf := nodes[0].Children[0]; !reflect.DeepEqual(wf.Workflow, &run) {
		t.Fatalf("workflow payload = %+v", wf.Workflow)
	}
}
