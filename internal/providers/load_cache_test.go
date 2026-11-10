package providers

import (
	"fmt"
	"os"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestLoadCacheReturnsIndependentRegistry(t *testing.T) {
	// Given a settled vault with a provider model and fallback list.
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"fallback":["ultra"]}`)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(vault.Path(), old, old); err != nil {
		t.Fatal(err)
	}
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	wantModel := reg.Providers["ultra"].Models["opus"]
	wantFallback := slices.Clone(reg.Fallback)
	// When a caller mutates the returned model map and appends to fallback.
	reg.Providers["ultra"].Models["opus"] = "x"
	reg.Fallback = append(reg.Fallback, "caller-only")
	next, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	// Then another Load returns the unchanged cached registry.
	if got := next.Providers["ultra"].Models["opus"]; got != wantModel {
		t.Errorf("Providers[ultra].Models[opus] = %q, want %q", got, wantModel)
	}
	if !slices.Equal(next.Fallback, wantFallback) {
		t.Errorf("Fallback = %v, want %v", next.Fallback, wantFallback)
	}
}

func TestRegistryCloneReferenceFields(t *testing.T) {
	// Given a registry with every reference field populated, including private declaration order.
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	reg := Registry{order: []string{"order"}}
	populateCloneReferences(reflect.ValueOf(&reg).Elem())
	// When the real Clone copies all nested provider and routing types.
	cloned := reg.Clone()
	// Then values are preserved and no map, slice or pointer field aliases its original.
	if !reflect.DeepEqual(&reg, cloned) {
		t.Fatal("Clone changed registry values")
	}
	assertCloneReferences(t, reflect.ValueOf(&reg), reflect.ValueOf(cloned), "Registry")
}

func populateCloneReferences(v reflect.Value) {
	switch v.Kind() {
	case reflect.Struct:
		for _, fv := range v.Fields() {
			if fv.CanSet() {
				populateCloneReferences(fv)
			}
		}
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		populateCloneReferences(v.Elem())
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		key := reflect.New(v.Type().Key()).Elem()
		value := reflect.New(v.Type().Elem()).Elem()
		populateCloneReferences(key)
		populateCloneReferences(value)
		v.SetMapIndex(key, value)
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 2))
		populateCloneReferences(v.Index(0))
	case reflect.String:
		v.SetString("fixture")
	}
}

func assertCloneReferences(t *testing.T, original, cloned reflect.Value, field string) {
	t.Helper()
	switch original.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice:
		if original.IsNil() || cloned.IsNil() {
			t.Fatalf("%s: reference field must be populated in original and clone", field)
		}
		if original.UnsafePointer() == cloned.UnsafePointer() {
			t.Errorf("%s: Clone aliases original %s", field, original.Kind())
		}
	}
	walkCloneReferences(t, original, cloned, field)
}

func walkCloneReferences(t *testing.T, original, cloned reflect.Value, field string) {
	t.Helper()
	switch original.Kind() {
	case reflect.Struct:
		for i := 0; i < original.NumField(); i++ {
			name := field + "." + original.Type().Field(i).Name
			assertCloneReferences(t, original.Field(i), cloned.Field(i), name)
		}
	case reflect.Pointer:
		assertCloneReferences(t, original.Elem(), cloned.Elem(), field)
	case reflect.Map:
		for _, key := range original.MapKeys() {
			name := fmt.Sprintf("%s[%v]", field, key)
			assertCloneReferences(t, original.MapIndex(key), cloned.MapIndex(key), name)
		}
	case reflect.Slice:
		for i := 0; i < original.Len(); i++ {
			name := fmt.Sprintf("%s[%d]", field, i)
			assertCloneReferences(t, original.Index(i), cloned.Index(i), name)
		}
	}
}
