package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
)

// CheckFile is the read-only audit: it reports why file would make Claude Code throw, nil when it is valid.
func CheckFile(file string) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	return Check(data)
}

// Check parses data as a JSON object and validates every key the schema documents; unknown keys pass.
func Check(data []byte) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return located(data, err)
	}
	if top == nil {
		return errors.New("not a JSON object")
	}
	for _, k := range slices.Sorted(maps.Keys(top)) {
		if sk, ok := SchemaKey(k); ok && !sk.Env {
			if err := sk.Validate(top[k]); err != nil {
				return err
			}
		}
	}
	return nil
}

// located prefixes a decode error with the line and column its byte offset falls on.
func located(data []byte, err error) error {
	var off int64
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syn):
		off = syn.Offset
	case errors.As(err, &typ):
		off = typ.Offset
	default:
		return err
	}
	before := data[:min(int(off), len(data))]
	line := bytes.Count(before, []byte("\n")) + 1
	col := len(before) - bytes.LastIndexByte(before, '\n')
	return fmt.Errorf("line %d col %d: %w", line, col, err)
}
