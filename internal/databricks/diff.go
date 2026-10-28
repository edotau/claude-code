package databricks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/edotau/claude-code/internal/secrets/shapes"
)

// redacted masks a credential value in a diff; redactedChanged marks one whose value differs across the two docs.
const (
	redacted        = "<redacted>"
	redactedChanged = "<redacted> (changed)"
)

// credWords are the name segments that mark a string leaf as a credential value.
var credWords = map[string]bool{
	"KEY": true, "TOKEN": true, "SECRET": true, "PASSWORD": true, "CREDENTIAL": true, "CREDENTIALS": true,
	"BEARER": true, "PAT": true, "AUTHORIZATION": true,
}

// camelGap splits lowerUpper boundaries so apiKey reads as api_Key.
var camelGap = regexp.MustCompile(`([a-z0-9])([A-Z])`)

// credLeaf reports a key whose name segment marks its string value as a credential (api_key, clientSecret).
func credLeaf(k string) bool {
	k = camelGap.ReplaceAllString(k, "${1}_${2}")
	for _, seg := range strings.FieldsFunc(strings.ToUpper(k), func(r rune) bool { return r == '_' || r == '-' || r == '.' }) {
		if credWords[seg] {
			return true
		}
	}
	return false
}

// maskPair returns copies of o and n with every string under a credential key masked; a value that
// differs between them is marked changed on the new side so the diff still shows the line.
func maskPair(o, n any, secret bool, key string) (any, any) {
	om, ook := o.(map[string]any)
	nm, nok := n.(map[string]any)
	if ook || nok {
		oo, nn := map[string]any{}, map[string]any{}
		for k, v := range om {
			oo[k] = v
		}
		for k, v := range nm {
			nn[k] = v
		}
		for k := range oo {
			ov, nv := oo[k], any(nil)
			if v, ok := nn[k]; ok {
				nv = v
			}
			ov, nv = maskPair(ov, nv, secret || strings.EqualFold(k, "headers"), k)
			oo[k] = ov
			if _, ok := nn[k]; ok {
				nn[k] = nv
			}
		}
		for k := range nn {
			if _, ok := oo[k]; ok {
				continue
			}
			_, nv := maskPair(nil, nn[k], secret || strings.EqualFold(k, "headers"), k)
			nn[k] = nv
		}
		if !ook {
			return o, nn
		}
		if !nok {
			return oo, n
		}
		return oo, nn
	}
	oa, ook := o.([]any)
	na, nok := n.([]any)
	if ook || nok {
		oo, nn := make([]any, len(oa)), make([]any, len(na))
		for i := range max(len(oa), len(na)) {
			var ov, nv any
			if i < len(oa) {
				ov = oa[i]
			}
			if i < len(na) {
				nv = na[i]
			}
			ov, nv = maskPair(ov, nv, secret, key)
			if i < len(oa) {
				oo[i] = ov
			}
			if i < len(na) {
				nn[i] = nv
			}
		}
		return oo, nn
	}
	if !secret && !credLeaf(key) {
		return o, n
	}
	os, oIsStr := o.(string)
	ns, nIsStr := n.(string)
	if oIsStr {
		o = redacted
	}
	if nIsStr {
		n = redacted
		if oIsStr && os != ns {
			n = redactedChanged
		}
	}
	return o, n
}

// normalize re-indents a parsed JSON document with sorted keys so formatting never shows as a change.
func normalize(v any) []string {
	if v == nil {
		return nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v) // v came from json.Unmarshal, so it always encodes
	return strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
}

// parseDoc decodes an overlay document; ok is false for empty or non-JSON input.
func parseDoc(doc []byte) (any, bool) {
	var v any
	if strings.TrimSpace(string(doc)) == "" || json.Unmarshal(doc, &v) != nil {
		return nil, false
	}
	return v, true
}

// diffInputs returns the masked line lists for both documents; non-JSON text falls back to shapes.Redact.
func diffInputs(oldDoc, newDoc []byte) ([]string, []string) {
	ov, ook := parseDoc(oldDoc)
	nv, nok := parseDoc(newDoc)
	if (ook || strings.TrimSpace(string(oldDoc)) == "") && (nok || strings.TrimSpace(string(newDoc)) == "") {
		ov, nv = maskPair(ov, nv, false, "")
		return normalize(ov), normalize(nv)
	}
	raw := func(doc []byte) []string {
		if strings.TrimSpace(string(doc)) == "" {
			return nil
		}
		return strings.Split(shapes.Redact(string(doc)), "\n")
	}
	return raw(oldDoc), raw(newDoc)
}

type edit struct {
	op   byte
	line string
}

// diffLines is an LCS line diff; the overlay is a few hundred lines, so the quadratic table is fine.
func diffLines(a, b []string) []edit {
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var edits []edit
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			edits = append(edits, edit{' ', a[i]})
			i++
			j++
		case j < len(b) && (i == len(a) || lcs[i][j+1] >= lcs[i+1][j]):
			edits = append(edits, edit{'+', b[j]})
			j++
		default:
			edits = append(edits, edit{'-', a[i]})
			i++
		}
	}
	return edits
}

// printDiff writes a line diff of two overlay documents (+/-, 2 lines of context); false when identical.
func printDiff(w io.Writer, oldDoc, newDoc []byte) bool {
	oldLines, newLines := diffInputs(oldDoc, newDoc)
	edits := diffLines(oldLines, newLines)
	const ctx = 2
	show := make([]bool, len(edits))
	changed := false
	for k, e := range edits {
		if e.op == ' ' {
			continue
		}
		changed = true
		for d := -ctx; d <= ctx; d++ {
			if k+d >= 0 && k+d < len(edits) {
				show[k+d] = true
			}
		}
	}
	gap := false
	for k, e := range edits {
		if !show[k] {
			gap = true
			continue
		}
		if gap {
			fmt.Fprintln(w, "  ...")
			gap = false
		}
		fmt.Fprintf(w, "%c %s\n", e.op, e.line)
	}
	return changed
}
