package databricks

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

var secretish = regexp.MustCompile(`(?i)token|secret|password|passwd|pat$|bearer|credential|apikey|key`)

// Candidate must be structurally unable to carry a secret: no secret-named string field, only presence flags.
func TestCandidateHasNoSecretBearingField(t *testing.T) {
	typ := reflect.TypeFor[Candidate]()
	for f := range typ.Fields() {
		if secretish.MatchString(f.Name) && f.Type.Kind() != reflect.Bool {
			t.Errorf("Candidate.%s (%s) looks secret-bearing", f.Name, f.Type)
		}
		switch f.Type.Kind() {
		case reflect.String, reflect.Bool:
		default:
			t.Errorf("Candidate.%s has kind %s; only string/bool fields are allowed", f.Name, f.Type.Kind())
		}
	}
}

func TestSecretValuesNeverAppearInCandidatesOrPlan(t *testing.T) {
	cs, err := Candidates(writeFixtureCfg(t))
	if err != nil {
		t.Fatal(err)
	}
	rows := ImportPlan(cs, nil, ImportOptions{})
	var sb strings.Builder
	for _, c := range cs {
		fmt.Fprintf(&sb, "%+v %#v\n", c, c)
		b, _ := json.Marshal(c)
		sb.Write(b)
	}
	for _, r := range rows {
		fmt.Fprintf(&sb, "%+v\n", r)
		if b, err := RowJSON(r); err == nil {
			sb.Write(b)
		}
	}
	for _, leak := range []string{"shh-beta", "dapi0123", "dapiDEFAULTLEAK", "leak.example"} {
		if strings.Contains(sb.String(), leak) {
			t.Errorf("%q leaked into candidates/plan output", leak)
		}
	}
}

func TestDefaultAndSettingsSectionsAreNeverCandidates(t *testing.T) {
	cs, err := Candidates(writeFixtureCfg(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if strings.EqualFold(c.Name, "default") || c.Name == "__settings__" {
			t.Errorf("section %q became a candidate", c.Name)
		}
	}
	if _, err := ReadSecrets(writeFixtureCfg(t), "__settings__"); err == nil {
		t.Error("__settings__ is readable as a secret source")
	}
}
