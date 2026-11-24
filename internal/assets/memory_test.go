package assets

import "testing"

func TestMemoryFramingSentinels(t *testing.T) {
	if got := MemoryRecallFooter(); got != "\nLoad one with: claude-code memory show <id>\n</memory-recall>" {
		t.Fatalf("recall footer changed: %q", got)
	}
	if got := MemoryRecallMarker(); got != "<memory-recall>" {
		t.Fatalf("recall marker changed: %q", got)
	}
	if got := MemoryRecallLine("", "body", true); got != "- [] body" {
		t.Fatalf("empty filename framing changed: %q", got)
	}
}

func TestMemoryFolderRolesFreshCopy(t *testing.T) {
	roles := MemoryFolderRoles()
	if len(roles) == 0 {
		t.Fatal("folder roles are empty")
	}
	want := roles[0]
	roles[0] = [2]string{"changed", "changed"}
	if got := MemoryFolderRoles()[0]; got != want {
		t.Fatalf("mutating a result changed cached roles: %v", got)
	}
}

func TestMemoryEmptyProcedurePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("empty embedded procedure must fail")
		}
	}()
	memoryProcedure([]byte(" \n\t"))
}
