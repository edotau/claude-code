package settings

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
)

// TestDiffInvalidJSON: malformed existing JSON (a trailing comma) must surface as an error wrapping
// ErrInvalidJSON, not a silent "no drift" or a generic error the caller can't branch on.
func TestDiffInvalidJSON(t *testing.T) {
	rendered, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = Diff([]byte(`{"a":1,}`), rendered)
	if err == nil {
		t.Fatal("Diff on trailing-comma JSON = nil error, want ErrInvalidJSON")
	}
	if !errors.Is(err, ErrInvalidJSON) {
		t.Errorf("Diff error = %v, want errors.Is(ErrInvalidJSON)", err)
	}
}

// TestDiffDetectsChangedValue: a hand-edited value that differs from the rendered template must be reported
// in differs, by key.
func TestDiffDetectsChangedValue(t *testing.T) {
	rendered, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(rendered, &obj); err != nil {
		t.Fatal(err)
	}
	obj["theme"] = json.RawMessage(`"light"`)
	existing, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}

	differs, foreign, err := Diff(existing, rendered)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(differs, "theme") {
		t.Errorf("differs = %v, want it to contain %q", differs, "theme")
	}
	if len(foreign) != 0 {
		t.Errorf("foreign = %v, want none", foreign)
	}
}

// TestDiffDetectsForeignKey: a key present in the existing file but absent from the rendered template is
// foreign — e.g. something /config wrote that the harness does not own — not drift.
func TestDiffDetectsForeignKey(t *testing.T) {
	rendered, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(rendered, &obj); err != nil {
		t.Fatal(err)
	}
	obj["verbose"] = json.RawMessage(`true`)
	existing, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}

	differs, foreign, err := Diff(existing, rendered)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(foreign, "verbose") {
		t.Errorf("foreign = %v, want it to contain %q", foreign, "verbose")
	}
	if len(differs) != 0 {
		t.Errorf("differs = %v, want none", differs)
	}
}

// TestDiffCleanAfterMerge: a file produced by Merge(existing, rendered) must diff clean against that same
// rendered template — formatting differences (key order, whitespace) must not register as drift.
func TestDiffCleanAfterMerge(t *testing.T) {
	rendered, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	merged, _, err := Merge([]byte(`{"verbose":true}`), rendered, nil)
	if err != nil {
		t.Fatal(err)
	}

	differs, foreign, err := Diff(merged, rendered)
	if err != nil {
		t.Fatal(err)
	}
	if len(differs) != 0 {
		t.Errorf("differs = %v, want none (Merge output against its own rendered template)", differs)
	}
	if !slices.Contains(foreign, "verbose") {
		t.Errorf("foreign = %v, want it to still contain the foreign key Merge preserved", foreign)
	}
}

// TestLocalDuplicates: a settings.local.json entry whose value equals the rendered template's value for the
// same key is a pointless duplicate worth flagging — it does nothing but risk drifting from the template.
func TestLocalDuplicates(t *testing.T) {
	rendered, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(rendered, &obj); err != nil {
		t.Fatal(err)
	}
	env := map[string]json.RawMessage{}
	if err := json.Unmarshal(obj["env"], &env); err != nil {
		t.Fatal(err)
	}
	var envKey string
	for k := range env {
		envKey = k
		break
	}
	if envKey == "" {
		t.Fatal("rendered template has no env keys to test against")
	}

	local := []byte(`{"env":{"` + envKey + `":` + string(env[envKey]) + `}}`)
	dups, err := LocalDuplicates(local, rendered)
	if err != nil {
		t.Fatal(err)
	}
	want := "env." + envKey
	if !slices.Contains(dups, want) {
		t.Errorf("LocalDuplicates = %v, want it to contain %q", dups, want)
	}
}

// TestLocalDuplicatesIgnoresDifferingValue: a settings.local.json entry whose value differs from the
// rendered template is an intentional per-session override, not a duplicate — it must not be reported.
func TestLocalDuplicatesIgnoresDifferingValue(t *testing.T) {
	rendered, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	local := []byte(`{"env":{"SOME_LOCAL_ONLY_VAR":"x"}}`)
	dups, err := LocalDuplicates(local, rendered)
	if err != nil {
		t.Fatal(err)
	}
	if len(dups) != 0 {
		t.Errorf("LocalDuplicates = %v, want none for a key the template never defines", dups)
	}
}
