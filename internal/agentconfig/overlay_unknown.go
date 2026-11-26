package agentconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ReportUnknownKey files one overlay_unknown_key failure per load; internal/worktree sets it (D3), nil is a no-op.
var ReportUnknownKey func(keys []string, origin string)

const maxUnknownPasses = 64

func reportUnknownKey(keys []string, origin string) {
	if ReportUnknownKey != nil && len(keys) > 0 {
		ReportUnknownKey(keys, origin)
	}
}

// unknownFieldKey extracts the key of an encoding/json unknown-field error (it has no typed error).
func unknownFieldKey(err error) (string, bool) {
	_, q, ok := strings.Cut(err.Error(), "json: unknown field ")
	if !ok {
		return "", false
	}
	if key, uerr := strconv.Unquote(q); uerr == nil {
		return key, true
	}
	return q, true
}

// dropStoredUnknown cuts unknown keys (any depth) from a stored overlay, warns once, files one entry (D3).
func dropStoredUnknown(raw []byte, origin string) []byte {
	var dropped []string
	for range maxUnknownPasses { // each pass removes bytes or stops, so it cannot loop
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		derr := dec.Decode(new(Overlay))
		if derr == nil {
			break
		}
		key, ok := unknownFieldKey(derr)
		if !ok {
			break
		}
		next, found := cutMember(raw, key)
		if !found {
			break
		}
		raw, dropped = next, append(dropped, key)
	}
	if len(dropped) > 0 {
		fmt.Fprintf(os.Stderr, "warning: stored overlay (%s) has unknown keys %s: ignored\n", origin, strings.Join(dropped, ","))
		reportUnknownKey(dropped, origin)
	}
	return raw
}

// cutMember removes the first member named key at any depth, touching no other byte; found is false when absent.
func cutMember(raw []byte, key string) (out []byte, found bool) {
	type frame struct{ obj, keyNext bool }
	dec := json.NewDecoder(bytes.NewReader(raw))
	var stack []frame
	for {
		prev := dec.InputOffset()
		tok, err := dec.Token()
		if err != nil {
			return raw, false
		}
		top := len(stack) - 1
		if d, ok := tok.(json.Delim); ok && (d == '{' || d == '[') {
			stack = append(stack, frame{obj: d == '{', keyNext: d == '{'})
			continue
		} else if ok {
			stack = stack[:top]
			top--
		}
		if top < 0 || !stack[top].obj {
			continue
		}
		if name, isStr := tok.(string); isStr && stack[top].keyNext {
			if name != key {
				stack[top].keyNext = false
				continue
			}
			var v json.RawMessage
			if dec.Decode(&v) != nil {
				return raw, false
			}
			return cutRange(raw, prev, int(dec.InputOffset())), true
		}
		stack[top].keyNext = true // a scalar or closed container finished this member's value
	}
}

// cutRange deletes the member between its key's lead-in at from and end, with one adjoining comma.
func cutRange(raw []byte, from int64, end int) []byte {
	start := int(from)
	for start < end && bytes.IndexByte([]byte(" \t\r\n,"), raw[start]) >= 0 {
		start++
	}
	j := end
	for j < len(raw) && bytes.IndexByte([]byte(" \t\r\n"), raw[j]) >= 0 {
		j++
	}
	if j < len(raw) && raw[j] == ',' {
		end = j + 1
	} else {
		k := start
		for k > 0 && bytes.IndexByte([]byte(" \t\r\n"), raw[k-1]) >= 0 {
			k--
		}
		if k > 0 && raw[k-1] == ',' {
			start = k - 1
		}
	}
	return append(raw[:start:start], raw[end:]...)
}
