package schemas

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// peek returns the document's "schema" number; a missing field is version 0.
func peek(raw []byte) (int, error) {
	var p struct {
		Schema *int `json:"schema"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return 0, err
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, errors.New("document is null, not an object")
	}
	if p.Schema == nil {
		return 0, nil
	}
	return *p.Schema, nil
}

// upgrade applies Migrate step by step from the document's version to f.Version.
func upgrade(f Format, raw []byte) (out []byte, from int, err error) {
	from, err = peek(raw)
	if err != nil {
		return nil, 0, err
	}
	if from > f.Version {
		return nil, from, fmt.Errorf("%s v%d > v%d: %w", f.Name, from, f.Version, ErrNewer)
	}
	out = raw
	for v := from; v < f.Version; v++ {
		if f.Migrate == nil {
			return nil, from, fmt.Errorf("%s v%d -> v%d: %w", f.Name, v, v+1, ErrNoMigration)
		}
		if out, err = f.Migrate(v, out); err != nil {
			return nil, from, fmt.Errorf("%s v%d -> v%d: %w", f.Name, v, v+1, err)
		}
	}
	return out, from, nil
}

// Decode unmarshals raw into dst, migrating older versions step by step; from is the version found.
func Decode(f Format, raw []byte, dst any) (from int, err error) {
	out, from, err := upgrade(f, raw)
	if err != nil {
		return from, err
	}
	return from, json.Unmarshal(out, dst)
}

// SameShape is a Migrate step for a version bump that only adds the "schema" stamp.
func SameShape(from int, raw []byte) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errors.New("document is null, not an object")
	}
	n, err := json.Marshal(from + 1)
	if err != nil {
		return nil, err
	}
	m["schema"] = n
	return json.Marshal(m)
}

// DecodeOK reports whether raw decodes at the current version; an unparsable or newer-schema document is a miss.
func DecodeOK(f Format, raw []byte, dst any) bool {
	_, err := Decode(f, raw, dst)
	return err == nil
}
