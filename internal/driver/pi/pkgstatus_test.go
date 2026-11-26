package pi

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestStatusIn(t *testing.T) {
	store := t.TempDir()
	skills := mustPkgs(t, "npm:sk@1.0.0")[0]
	code := mustPkgs(t, "npm:cd@1.0.0")[0]
	mk := func(p Package, manifest string) {
		slot := filepath.Join(store, slotName(p))
		writeFile(t, filepath.Join(packageDir(slot, p), "package.json"), manifest)
		writeFile(t, filepath.Join(slot, specMarker), p.Spec)
	}
	mk(skills, `{"name":"sk","version":"1.0.0","pi":{"skills":["./s"]}}`)
	mk(code, `{"name":"cd","version":"1.0.0","pi":{"extensions":["./e.ts"]}}`)
	stale := mustPkgs(t, "npm:old@2.0.0")[0]
	writeFile(t, filepath.Join(store, slotName(stale), specMarker), stale.Spec)

	if got, _ := statusIn(t.TempDir(), "", nil, nil); len(got) != 0 {
		t.Fatalf("empty: %v", got)
	}
	got, _ := statusIn(store, "", []Package{skills, code}, nil)
	if got[0].Problem != "" || got[0].ShipsCode || strings.Join(got[0].Kinds, ",") != "skills" {
		t.Errorf("skills: %+v", got[0])
	}
	if !strings.Contains(got[1].Problem, "not approved") || !got[2].Stale {
		t.Errorf("unapproved/stale: %+v %+v", got[1], got[2])
	}
	if got, _ = statusIn(store, "", []Package{skills, code, stale}, []Package{code}); got[1].Problem != "" || !got[1].Approved {
		t.Errorf("approved: %+v", got[1])
	}
	missing := mustPkgs(t, "npm:nope@1.0.0")[0]
	if got, _ = statusIn(store, "", []Package{missing}, nil); !strings.Contains(got[0].Problem, "not installed yet") {
		t.Errorf("missing: %+v", got[0])
	}
}

// Doctor runs the launch's pin check (a git commit too) and reports an unusable store even with nothing pinned.
func TestStatusInPinAndStoreErrors(t *testing.T) {
	store := t.TempDir()
	g := mustPkgs(t, "git:github.com/o/r@"+strings.Repeat("a", 40))[0]
	slot := filepath.Join(store, slotName(g))
	writeFile(t, filepath.Join(packageDir(slot, g), "package.json"), `{"name":"g","pi":{"skills":["./s"]}}`)
	writeFile(t, filepath.Join(slot, specMarker), g.Spec)
	writeFile(t, filepath.Join(packageDir(slot, g), ".git", "HEAD"), strings.Repeat("b", 40))
	if got, _ := statusIn(store, "", []Package{g}, nil); !got[0].VersionMismatch {
		t.Errorf("git commit drift not reported: %+v", got[0])
	}
	notDir := filepath.Join(t.TempDir(), "store")
	writeFile(t, notDir, "x")
	if _, err := statusIn(notDir, "", nil, nil); err == nil {
		t.Error("a non-directory store reported healthy")
	}
}
