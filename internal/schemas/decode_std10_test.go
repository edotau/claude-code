package schemas

import "testing"

// TestStd10DecodeNullIsErrorNotPanic: a `null` document is rejected, never a nil-map panic.
func TestStd10DecodeNullIsErrorNotPanic(t *testing.T) {
	f := Format{Name: "std10-null", Version: 1, Migrate: SameShape}
	var got map[string]any
	if _, err := Decode(f, []byte("null"), &got); err == nil {
		t.Fatal("Decode(null) returned nil error")
	}
	if _, err := SameShape(0, []byte("null")); err == nil {
		t.Fatal("SameShape(0, null) returned nil error")
	}
}
