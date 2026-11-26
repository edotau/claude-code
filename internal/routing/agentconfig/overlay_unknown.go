package agentconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
)

// ReportUnknownKey files one overlay_unknown_key failure per load; internal/worktree sets it (D3), nil is a no-op.
var ReportUnknownKey func(keys []string, origin string)

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
	raw, dropped := cutUnknownMembers(raw, true)
	if len(dropped) > 0 {
		names := strings.Join(dropped[:min(10, len(dropped))], ",")
		if len(dropped) > 10 {
			names += fmt.Sprintf(" +%d more", len(dropped)-10)
		}
		fmt.Fprintf(os.Stderr, "warning: stored overlay (%s) has unknown keys %s: ignored\n", origin, names)
		reportUnknownKey(dropped, origin)
	}
	return raw
}

// cutUnknownMembers visits each typed object once; arbitrary map values and agent definitions stay opaque.
func cutUnknownMembers(raw []byte, root bool) ([]byte, []string) {
	known := []string{"profiles", "open"}
	if root {
		known = append(overlayNames(), "files")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return raw, nil
	}
	out := []byte{'{'}
	var dropped []string
	for dec.More() {
		start := int(dec.InputOffset())
		tok, err := dec.Token()
		if err != nil {
			return raw, nil
		}
		name, ok := tok.(string)
		var value json.RawMessage
		if !ok || dec.Decode(&value) != nil {
			return raw, nil
		}
		if !slices.ContainsFunc(known, func(k string) bool { return strings.EqualFold(name, k) }) {
			dropped = append(dropped, name)
			continue
		}
		prefix := bytes.TrimLeft(raw[start:int(dec.InputOffset())-len(value)], " \t\r\n,")
		if root && strings.EqualFold(name, "fence") {
			var nested []string
			value, nested = cutUnknownMembers(value, false)
			dropped = append(dropped, nested...)
		}
		if len(out) > 1 {
			out = append(out, ',')
		}
		out = append(append(out, prefix...), value...)
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return raw, nil
	}
	return append(out, raw[int(dec.InputOffset())-1:]...), dropped
}
