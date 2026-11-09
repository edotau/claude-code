package render

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"

	"github.com/edotau/claude-code/internal/textutil"
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

// DecodeRawObject decodes raw as a JSON object, keeping every key as raw bytes; blank decodes to empty, JSON null is rejected.
func DecodeRawObject(raw []byte) (map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m == nil { // json null unmarshals to a nil map: not an object, so refuse to overwrite it
		return nil, errors.New("not a JSON object")
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
		merged[event] = spliceEvent(items, rendered[event], classify)
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

// spliceEvent drops each item's harness-owned part per classify and splices block in at the first owned position.
func spliceEvent[T any](items, block []T, classify func(T) (*T, bool)) []T {
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
	result := make([]T, 0, len(out)+len(block))
	result = append(result, out[:insertAt]...)
	result = append(result, block...)
	result = append(result, out[insertAt:]...)
	return result
}

// MergeHooksTop merges body's "hooks" over existingRaw's per classify; next is the existing top level, updated.
func MergeHooksTop(existingRaw, body []byte, classify func(json.RawMessage) (*json.RawMessage, bool)) (next, renderedTop map[string]json.RawMessage, err error) {
	existingTop, err := DecodeRawObject(existingRaw)
	if err != nil {
		return nil, nil, fmt.Errorf("existing file: %w", err)
	}
	renderedTop, err = DecodeRawObject(body)
	if err != nil {
		return nil, nil, err
	}
	existingHooks, err := DecodeRawEvents(existingTop["hooks"])
	if err != nil {
		return nil, nil, fmt.Errorf("existing file: %w", err)
	}
	renderedHooks, err := DecodeRawEvents(renderedTop["hooks"])
	if err != nil {
		return nil, nil, err
	}
	hooksRaw, err := textutil.MarshalNoEscape(MergeHookEvents(existingHooks, renderedHooks, classify))
	if err != nil {
		return nil, nil, err
	}
	next = maps.Clone(existingTop)
	if next == nil {
		next = map[string]json.RawMessage{}
	}
	next["hooks"] = hooksRaw
	return next, renderedTop, nil
}
