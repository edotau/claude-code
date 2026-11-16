package tree

import (
	"testing"
)

func TestReadSubagentMetaRejectsPartialDecode(t *testing.T) {
	if got, _ := parseMeta([]byte(`{"finished":true,"spawnDepth":"invalid"}`)); got != (subagentMeta{}) {
		t.Fatalf("metadata = %+v, want zero value for invalid sidecar", got)
	}
}

func TestWorkflowManifestNameUsesMeta(t *testing.T) {
	manifest := workflowManifest{Script: "const task = { name: 'worker' }; const meta = { name: 'release' };"}
	if got := manifest.name(); got != "release" {
		t.Fatalf("workflow name = %q, want meta.name release rather than task.name worker", got)
	}
}
