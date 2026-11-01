package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// RenderHooksFile writes body to path, merging it over whatever is already there when merge is
// non-nil: a nil merge overwrites outright (nothing currently needs that for a hooks file).
func RenderHooksFile(path string, body []byte, merge func(existing, body []byte) ([]byte, error), o RenderOptions, out io.Writer, label string) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("%s: %w", label, err)
	}
	existed := err == nil
	next := body
	if merge != nil {
		if next, err = merge(raw, body); err != nil {
			return false, fmt.Errorf("%s: %w", label, err)
		}
	}
	return CommitFile(path, raw, append(next, '\n'), existed, o, out, label)
}

// DecodeRawObject decodes raw as a JSON object into map[string]json.RawMessage, preserving every key
// (known or not) as raw bytes; blank/absent raw decodes to an empty map rather than an error.
func DecodeRawObject(raw []byte) (map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	return m, nil
}

// DecodeRawEvents decodes a "hooks" field (event name -> array of raw group/entry objects); a nil or
// absent field decodes to an empty map.
func DecodeRawEvents(raw json.RawMessage) (map[string][]json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string][]json.RawMessage{}, nil
	}
	var m map[string][]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string][]json.RawMessage{}
	}
	return m, nil
}

// MergeHookEvents keeps every existing item of each event in its original relative position, dropping
// an item's harness-owned part per classify; the freshly rendered block is spliced in as a whole at the
// position of the FIRST existing item classify found harness-owned (prepended when none was); an event
// left with nothing is dropped. Idempotent: merging an already-merged event over the same rendered
// content reproduces it byte-for-byte, since the rendered block is itself wholly harness-owned next time
// round. Shared by the codex and copilot hook mergers.
func MergeHookEvents[T any](existing, rendered map[string][]T, classify func(T) (*T, bool)) map[string][]T {
	merged := map[string][]T{}
	for event, items := range existing {
		var out []T
		insertAt := -1
		for _, item := range items {
			remainder, harnessOwned := classify(item)
			if harnessOwned && insertAt == -1 {
				insertAt = len(out)
			}
			if remainder != nil {
				out = append(out, *remainder)
			}
		}
		if insertAt == -1 {
			insertAt = 0 // no harness-owned item in this event: prepend, as if nothing existed before
		}
		block := rendered[event]
		result := make([]T, 0, len(out)+len(block))
		result = append(result, out[:insertAt]...)
		result = append(result, block...)
		result = append(result, out[insertAt:]...)
		merged[event] = result
	}
	for event, items := range rendered {
		if _, ok := existing[event]; !ok {
			merged[event] = items
		}
	}
	for event, items := range merged {
		if len(items) == 0 {
			delete(merged, event)
		}
	}
	return merged
}
