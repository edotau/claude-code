package tree

import "encoding/json"

// Kind names what a Node stands for: a session, one of its subagents, or a dispatch job.
type Kind string

const (
	KindSession  Kind = "session"
	KindSubagent Kind = "subagent"
	KindDispatch Kind = "dispatch"
	KindWorkflow Kind = "workflow"
)

// Node is one row of the tree; a folded carrier's id is Via and its Dispatch payload is the carrier's folded row.
type Node struct {
	Kind     Kind         `json:"kind"`
	ID       string       `json:"id"`
	Depth    int          `json:"depth"`
	Via      string       `json:"via,omitempty"`
	Agent    string       `json:"agent,omitempty"`
	Children []Node       `json:"children,omitempty"`
	Session  *Session     `json:"-"`
	Subagent *Subagent    `json:"-"`
	Dispatch *Dispatch    `json:"-"`
	Workflow *WorkflowRun `json:"-"`
}

// MarshalJSON is the listing's schema plus its node tree, so the JSON and the printed tree cannot disagree.
func (t Tree) MarshalJSON() ([]byte, error) {
	type plain Tree
	return json.Marshal(struct {
		plain
		Nodes []Node `json:"nodes,omitempty"`
	}{plain: plain(t), Nodes: Build(t.Sessions, t.Dispatches)})
}

// Build follows printed order; a carrier is never a node, its newest child takes its place.
func Build(sessions []Session, records []Dispatch) []Node {
	var nodes []Node
	for _, active := range []bool{true, false} {
		if !active {
			nodes = append(nodes, dispatchNodes(records, 0)...)
		}
		for _, repo := range groupByRepo(sessions, active) {
			for _, session := range repo.sessions {
				nodes = append(nodes, sessionNode(session))
			}
		}
	}
	return nodes
}

func sessionNode(session Session) Node {
	n := Node{Kind: KindSession, ID: session.ID, Session: &session}
	for _, sub := range session.Subagents {
		n.Children = append(n.Children, subagentNode(sub, 1))
	}
	for _, run := range session.Workflows {
		workflow := Node{Kind: KindWorkflow, ID: run.ID, Agent: run.Name, Depth: 1, Workflow: &run}
		forEachRunAgent(run, func(sub Subagent) {
			workflow.Children = append(workflow.Children, subagentNode(sub, 2))
		})
		n.Children = append(n.Children, workflow)
	}
	return n
}

func subagentNode(sub Subagent, depth int) Node {
	return Node{
		Kind: KindSubagent, ID: sub.ID, Depth: depth, Agent: sub.Type, Subagent: &sub,
		Children: dispatchNodes(sub.Dispatches, depth+1),
	}
}

func dispatchNodes(records []Dispatch, depth int) []Node {
	parents := nodeParents(records)
	children := make([][]int, len(records))
	var roots []int
	for i, parent := range parents {
		if parent < 0 {
			roots = append(roots, i)
		} else {
			children[parent] = append(children[parent], i)
		}
	}
	var nodes []Node
	for _, root := range roots {
		n := dispatchNode(records, children, root)
		setNodeDepth(&n, depth)
		nodes = append(nodes, n)
	}
	return nodes
}

func nodeParents(records []Dispatch) []int {
	byID := make(map[string]int, len(records))
	for i, d := range records {
		byID[d.ID] = i
	}
	parents := make([]int, len(records))
	for i, d := range records {
		parent, ok := byID[d.Parent]
		parents[i] = -1
		if ok && d.Parent != "" && parent != i {
			parents[i] = parent
		}
	}
	breakNodeCycles(parents)
	return parents
}

func breakNodeCycles(parents []int) {
	state := make([]uint8, len(parents))
	for start := range parents {
		if state[start] != 0 {
			continue
		}
		var path []int
		at := start
		for at >= 0 && state[at] == 0 {
			state[at] = 1
			path = append(path, at)
			at = parents[at]
		}
		if at >= 0 && state[at] == 1 {
			for next := parents[at]; next != at; {
				parent := parents[next]
				parents[next] = -1
				next = parent
			}
			parents[at] = -1
		}
		for _, i := range path {
			state[i] = 2
		}
	}
}

func dispatchNode(records []Dispatch, children [][]int, i int) Node {
	d := records[i]
	n := Node{Kind: KindDispatch, ID: d.ID, Agent: d.Agent, Dispatch: &d}
	for _, child := range children[i] {
		n.Children = append(n.Children, dispatchNode(records, children, child))
	}
	if !d.carry || len(n.Children) == 0 {
		return n
	}
	d, n.ID = foldNodeDispatch(records, children, i) // n.Dispatch points at d, so it now holds the folded row
	n.Via, n.Agent = d.ID, d.Agent
	var descendants []Node
	for _, child := range n.Children {
		descendants = append(descendants, child.Children...)
	}
	n.Children = descendants
	return n
}

func foldNodeDispatch(records []Dispatch, children [][]int, i int) (Dispatch, string) {
	d := records[i]
	for records[i].carry && len(children[i]) > 0 {
		worker := children[i][0]
		for _, child := range children[i][1:] {
			if records[child].ID > records[worker].ID {
				worker = child
			}
		}
		d = foldInto(d, records[worker])
		i = worker
	}
	return d, records[i].ID
}

func setNodeDepth(n *Node, depth int) {
	n.Depth = depth
	for i := range n.Children {
		setNodeDepth(&n.Children[i], depth+1)
	}
}
