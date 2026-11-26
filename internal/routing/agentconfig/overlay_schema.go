package agentconfig

import (
	"encoding/json"
	"reflect"
	"strings"

	"github.com/edotau/claude-code/internal/routing/route"
)

type fieldKind string

const (
	kindString  fieldKind = "string"
	kindBool    fieldKind = "boolean"
	kindInt     fieldKind = "integer"
	kindNumber  fieldKind = "number"
	kindList    fieldKind = "[]string"
	kindStrings fieldKind = "map[string]string"
	kindFence   fieldKind = "fence"
	kindObject  fieldKind = "object"
)

type overlayField struct {
	name string
	kind fieldKind
}

// overlayFields is the Overlay struct's JSON keys in order, walked once; an unmapped field type panics at init.
var overlayFields = walkOverlay(reflect.TypeFor[Overlay]())

func walkOverlay(t reflect.Type) []overlayField {
	var out []overlayField
	for f := range t.Fields() {
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			out = append(out, walkOverlay(f.Type)...)
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" || name == "files" {
			continue
		}
		out = append(out, overlayField{name, kindOf(f.Type)})
	}
	return out
}

var scalarKinds = map[reflect.Kind]fieldKind{
	reflect.String: kindString, reflect.Bool: kindBool, reflect.Int: kindInt, reflect.Float64: kindNumber,
}

func kindOf(t reflect.Type) fieldKind {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == reflect.TypeFor[route.Fence]() {
		return kindFence
	}
	if t == reflect.TypeFor[json.RawMessage]() {
		return kindObject
	}
	if k, ok := scalarKinds[t.Kind()]; ok {
		return k
	}
	switch {
	case t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.String:
		return kindList
	case t.Kind() == reflect.Map && t.Key().Kind() == reflect.String && t.Elem().Kind() == reflect.String:
		return kindStrings
	}
	panic("agentconfig: overlay field type " + t.String() + " has no schema kind")
}

// overlayNames lists the overlay's JSON keys in struct order.
func overlayNames() []string {
	out := make([]string, len(overlayFields))
	for i, f := range overlayFields {
		out[i] = f.name
	}
	return out
}
