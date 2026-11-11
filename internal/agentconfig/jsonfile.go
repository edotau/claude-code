package agentconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"

	"github.com/edotau/claude-code/internal/textutil"
)

// ReadExisting reads path; a missing file is (nil, false, nil), any other read failure is an error.
func ReadExisting(path string) (raw []byte, existed bool, err error) {
	raw, err = os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

// CommitJSONObject reads path as a JSON object, lets mutate edit it, and commits the result. An unreadable or
// non-object file is skipped with a "label: skipped" note and left untouched; mutate returning false writes nothing.
func CommitJSONObject(path string, o RenderOptions, out io.Writer, label string, mutate func(obj map[string]any, existed bool) bool) (bool, error) {
	raw, existed, err := ReadExisting(path)
	if err != nil {
		fmt.Fprintf(out, "%s: skipped: cannot read %s: %v\n", label, path, err)
		return false, nil
	}
	obj := map[string]any{}
	if existed {
		if err := json.Unmarshal(raw, &obj); err != nil {
			fmt.Fprintf(out, "%s: skipped: %s is not plain JSON: %v\n", label, path, err)
			return false, nil
		}
		if obj == nil { // "null" decodes without error but leaves obj nil, not an empty object
			fmt.Fprintf(out, "%s: skipped: %s is not plain JSON: not an object\n", label, path)
			return false, nil
		}
	}
	if !mutate(obj, existed) {
		return false, nil
	}
	// NoEscape leaves <, > and & raw: an existing file holding \u003c escapes reads as drift and is rewritten once.
	body, err := textutil.MarshalIndentNoEscape(obj)
	if err != nil {
		return false, fmt.Errorf("%s: %w", label, err)
	}
	return CommitFile(path, raw, append(body, '\n'), existed, o, out, label)
}

// RenderHooksFile writes body to path, merged over whatever is already there.
func RenderHooksFile(path string, body []byte, merge func(existing, body []byte) ([]byte, error), o RenderOptions, out io.Writer, label string) (bool, error) {
	raw, existed, err := ReadExisting(path)
	if err != nil {
		return false, fmt.Errorf("%s: %w", label, err)
	}
	next, err := merge(raw, body)
	if err != nil {
		return false, fmt.Errorf("%s: %w", label, err)
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

// decodeRawEvents decodes a "hooks" field (event name -> array of raw group/entry objects); a nil or
// absent field decodes to an empty map.
func decodeRawEvents(raw json.RawMessage) (map[string][]json.RawMessage, error) {
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

// mergeHookEvents keeps every existing item of each event in its original relative position, dropping
// an item's harness-owned part per classify; the freshly rendered block is spliced in as a whole at the
// position of the FIRST existing item classify found harness-owned (prepended when none was); an event
// left with nothing is dropped. Idempotent: merging an already-merged event over the same rendered
// content reproduces it byte-for-byte, since the rendered block is itself wholly harness-owned next time
// round. Shared by the codex and copilot hook mergers.
func mergeHookEvents[T any](existing, rendered map[string][]T, classify func(T) (*T, bool)) map[string][]T {
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
	existingHooks, err := decodeRawEvents(existingTop["hooks"])
	if err != nil {
		return nil, nil, fmt.Errorf("existing file: %w", err)
	}
	renderedHooks, err := decodeRawEvents(renderedTop["hooks"])
	if err != nil {
		return nil, nil, err
	}
	hooksRaw, err := textutil.MarshalNoEscape(mergeHookEvents(existingHooks, renderedHooks, classify))
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
