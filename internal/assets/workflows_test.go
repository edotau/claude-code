package assets

import (
	"crypto/sha256"
	"fmt"
	"testing"
)

func TestWorkflowBlockBytes(t *testing.T) {
	for _, tc := range []struct {
		name, source, want string
	}{
		{"stepof", WorkflowStepOfSource(), "77fd9df09c3f5cf30d535b6633edfd22b807577a854ca4c1c129606aef1f4117"},
		{"stages", WorkflowStagesSource(), "6c1c80f09464d50011caa929e22c0c2509ed92892fcf8b41b64739a3b7174b6e"},
	} {
		if got := fmt.Sprintf("%x", sha256.Sum256([]byte(tc.source))); got != tc.want {
			t.Errorf("%s: sha256 %s, want pre-migration %s", tc.name, got, tc.want)
		}
	}
}
