package tree

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestReadSubagentMetaRejectsPartialDecode(t *testing.T) {
	transcript := filepath.Join(t.TempDir(), "agent-partial.jsonl")
	writeFixFixture(t, strings.TrimSuffix(transcript, ".jsonl")+".meta.json", `{"finished":true,"spawnDepth":"invalid"}`)
	if got := readSubagentMeta(transcript); got != (subagentMeta{}) {
		t.Fatalf("metadata = %+v, want zero value for invalid sidecar", got)
	}
}

func TestWorkflowManifestNameUsesMeta(t *testing.T) {
	manifest := workflowManifest{Script: "const task = { name: 'worker' }; const meta = { name: 'release' };"}
	if got := manifest.name(); got != "release" {
		t.Fatalf("workflow name = %q, want meta.name release rather than task.name worker", got)
	}
}
