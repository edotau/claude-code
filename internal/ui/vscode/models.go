package vscode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
)

const (
	modelsFileName = "chatLanguageModels.json"
	groupPrefix    = "Databricks "                // groups this merge owns; every other provider is the user's
	proxyKeyRef    = "${input:harnessProxyToken}" // VS Code prompts once and keeps the value in its secret storage
	maxOutputCap   = 32_000                       // the output cap current Claude/GPT tiers accept (copilot's too)
	vendorEndpoint = "customendpoint"
)

// wireAPI maps a router wire to VS Code's apiType and the full /p/<profile> path that serves it.
var wireAPI = map[string]struct{ apiType, path string }{
	providers.ProtocolAnthropic: {"messages", "/v1/messages"},
	providers.ProtocolOpenAI:    {"chat-completions", "/" + providers.ProtocolOpenAI + "/chat/completions"},
	providers.ProtocolResponses: {"responses", "/" + providers.ProtocolResponses + "/responses"},
}

// endpointModel is one chatLanguageModels.json model entry (Custom Endpoint reference).
type endpointModel struct {
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	URL               string            `json:"url"`
	APIType           string            `json:"apiType"`
	ToolCalling       bool              `json:"toolCalling"`
	Vision            bool              `json:"vision"`
	ContextWindow     int               `json:"contextWindow"`
	MaxOutputTokens   int               `json:"maxOutputTokens"`
	StatefulResponses *bool             `json:"statefulResponses,omitempty"`
	RequestHeaders    map[string]string `json:"requestHeaders"`
}

// endpointGroup is one provider entry: a model-picker group.
type endpointGroup struct {
	Name    string          `json:"name"`
	Vendor  string          `json:"vendor"`
	APIKey  string          `json:"apiKey"`
	APIType string          `json:"apiType"`
	Models  []endpointModel `json:"models"`
}

// modelGroups is what the merge writes: one group per profile, and the profiles whose listing was cold.
type modelGroups struct {
	Groups []endpointGroup
	Cold   []string // their existing group, if any, is kept as is
}

// routerBase is a seam for tests: the running proxy probe.
var routerBase = runningRouterBase

// runningRouterBase is the running proxy's base (its port may have moved), else the default port.
func runningRouterBase() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	port := gateway.DefaultPort
	if st, ok := gateway.Running(ctx); ok {
		port = st.Port
	}
	return gateway.ViaBase(gateway.Base(port))
}

// buildDatabricksGroups lists every signed-in Databricks serving profile's cached READY chat endpoints, no network.
func buildDatabricksGroups() (modelGroups, error) {
	reg, err := providers.Load()
	if err != nil {
		return modelGroups{}, err
	}
	var out modelGroups
	base := ""
	for _, name := range reg.ProfilesOf(providers.GroupDatabricks) {
		p := reg.Providers[name]
		if reg.ServingRow(name) != nil || p.Auth.Login != "" {
			continue
		}
		if !providers.CredentialPresent(p) { // signed out: keep any group an earlier sign-in wrote, say nothing
			out.Cold = append(out.Cold, name)
			continue
		}
		snap, ok := serving.Recall(p)
		if !ok {
			fmt.Fprintf(os.Stderr, "claude-code: vscode models: %s has no cached listing, group skipped "+
				"(claude-code preflight --discover %s)\n", name, name)
			out.Cold = append(out.Cold, name)
			continue
		}
		if base == "" {
			base = routerBase()
		}
		out.Groups = append(out.Groups, profileGroup(p, snap.Models, gateway.SessionBase(base, name)))
	}
	return out, nil
}

// profileGroup maps p's READY chat endpoints to one group; an endpoint p has no route for is left out.
func profileGroup(p *providers.Provider, infos []providers.ModelInfo, session string) endpointGroup {
	g := endpointGroup{Name: groupPrefix + p.Name, Vendor: vendorEndpoint, APIKey: proxyKeyRef,
		APIType: wireAPI[providers.ProtocolOpenAI].apiType, Models: []endpointModel{}}
	for _, info := range infos {
		if !info.ReadyChat() {
			continue
		}
		wire, ok, _ := providers.WireFor(p, info.ID)
		if !ok {
			continue
		}
		g.Models = append(g.Models, endpointEntry(p, info, wire, session))
	}
	return g
}

// endpointEntry is one model: the full endpoint name as id and label, routed through the proxy session base.
func endpointEntry(p *providers.Provider, info providers.ModelInfo, wire, session string) endpointModel {
	api := wireAPI[wire]
	window, output := providers.ContextWindow(p, info.ID), maxOutputCap
	if l, ok := models.Limits(info.ID); ok { // catalog limits only lower the window and the cap
		window, output = minPositive(window, l.Context), minPositive(output, l.Output)
	}
	claude := models.FamilyKind(info.ID) == models.FamilyClaude
	m := endpointModel{ID: info.ID, Name: info.ID, URL: session + api.path, APIType: api.apiType, ToolCalling: true,
		Vision: claude || strings.Contains(info.Capabilities, "vision"), ContextWindow: window, MaxOutputTokens: output,
		RequestHeaders: map[string]string{gateway.AgentHeader: "vscode"}}
	if wire == providers.ProtocolResponses {
		stateless := false // Databricks keeps no prior responses: send full history every turn
		m.StatefulResponses = &stateless
	}
	return m
}

func minPositive(a, b int) int {
	if b > 0 {
		return min(a, b)
	}
	return a
}

// syncModels merges the Databricks groups into dest, returning "<group> (<n> models)" per group; check never writes.
func syncModels(dest string, check bool) (mergeResult, []string, error) {
	mg, err := buildDatabricksGroups()
	if err != nil {
		return mergeResult{}, nil, fmt.Errorf("models: %w", err)
	}
	var summary []string
	for _, g := range mg.Groups {
		summary = append(summary, fmt.Sprintf("%s (%d models)", g.Name, len(g.Models)))
	}
	if check {
		result, err := mergeModels(dest, mg, true)
		return result, summary, err
	}
	var result mergeResult
	err = withLock("vscode-models.lock", dest, func() (mergeErr error) {
		result, mergeErr = mergeModels(dest, mg, false)
		return mergeErr
	})
	return result, summary, err
}

func mergeModels(dest string, mg modelGroups, check bool) (mergeResult, error) {
	var base []json.RawMessage
	rawBase, err := readBase(dest, "chat models", &base)
	if err != nil {
		return mergeResult{}, err
	}
	body, err := mergedModels(rawBase, base, mg)
	if err != nil {
		return mergeResult{}, err
	}
	if sameJSON(rawBase, body) || (len(rawBase) == 0 && len(base) == 0 && len(mg.Groups) == 0) {
		return mergeResult{}, nil
	}
	if check {
		return mergeResult{Changed: true}, nil
	}
	return writeGuarded(dest, "chat models", rawBase, body)
}

// ownsGroup is true for a group this merge wrote: the Databricks prefix and the proxy token reference, both.
func ownsGroup(raw json.RawMessage) (profile string, ours bool) {
	var head struct{ Name, APIKey string }
	_ = json.Unmarshal(raw, &head) // a non-object entry is foreign: kept
	profile, prefixed := strings.CutPrefix(head.Name, groupPrefix)
	return profile, prefixed && head.APIKey == proxyKeyRef
}

// mergedModels keeps each foreign entry's original text (comments too) and a cold group, then appends the groups.
func mergedModels(rawBase []byte, base []json.RawMessage, mg modelGroups) ([]byte, error) {
	head, elems, tail := splitArray(rawBase)
	if len(elems) != len(base) {
		return nil, fmt.Errorf("chat models: cannot split %d entries in the live file", len(base))
	}
	items := make([][]byte, 0, len(base)+len(mg.Groups))
	for i, raw := range base {
		if profile, ours := ownsGroup(raw); !ours || slices.Contains(mg.Cold, profile) {
			items = append(items, bytes.TrimSpace(elems[i]))
		}
	}
	for _, g := range mg.Groups {
		b, err := json.MarshalIndent(g, "  ", "  ")
		if err != nil {
			return nil, err
		}
		items = append(items, b)
	}
	return renderArray(head, items, tail), nil
}

// renderArray lays items out one per line under head (a header comment) and above tail (comments before the ']').
func renderArray(head string, items [][]byte, tail string) []byte {
	var b bytes.Buffer
	if head != "" {
		b.WriteString(head + "\n")
	}
	b.WriteByte('[')
	for i, it := range items {
		if i < len(items)-1 {
			e := valueEnd(it)
			it = slices.Concat(it[:e], []byte(","), it[e:]) // before a trailing // comment, which would swallow it
		}
		b.WriteString("\n  ")
		b.Write(it)
	}
	if tail != "" {
		b.WriteString("\n  " + tail)
	}
	if len(items) > 0 || tail != "" {
		b.WriteByte('\n')
	}
	b.WriteString("]\n")
	return b.Bytes()
}

// skipToken is the index past the string or comment starting at b[i] (a line comment stops at its newline), else i.
func skipToken(b []byte, i int) int {
	switch {
	case b[i] == '"':
		for i++; i < len(b) && b[i] != '"'; i++ {
			if b[i] == '\\' {
				i++
			}
		}
		return min(i+1, len(b))
	case bytes.HasPrefix(b[i:], []byte("//")):
		if n := bytes.IndexByte(b[i:], '\n'); n >= 0 {
			return i + n
		}
		return len(b)
	case bytes.HasPrefix(b[i:], []byte("/*")):
		if n := bytes.Index(b[i+2:], []byte("*/")); n >= 0 {
			return i + n + 4
		}
		return len(b)
	}
	return i
}

// valueEnd is the index past b's last byte that is neither whitespace nor inside a comment.
func valueEnd(b []byte) int {
	end := 0
	for i := 0; i < len(b); {
		j := skipToken(b, i)
		switch {
		case j == i:
			j = i + 1
			if strings.IndexByte(" \t\r\n", b[i]) < 0 {
				end = j
			}
		case b[i] == '"':
			end = j
		}
		i = j
	}
	return end
}

// splitArray cuts a JSONC array into its header comment, comma-less entries (comments kept) and trailing comments.
func splitArray(raw []byte) (head string, elems [][]byte, tail string) {
	open := openBracket(raw)
	if open < 0 {
		return "", nil, ""
	}
	depth, start := 1, open+1
	for i := start; i < len(raw); {
		if j := skipToken(raw, i); j > i {
			i = j
			continue
		}
		depth += depthDelta(raw[i])
		switch {
		case depth == 0:
			elems, tail = closeEntries(elems, raw[start:i])
			return string(bytes.TrimSpace(raw[:open])), elems, tail
		case raw[i] == ',' && depth == 1:
			elems, start = append(elems, raw[start:i]), i+1
		}
		i++
	}
	return "", nil, ""
}

// openBracket is the index of raw's first '[' outside a comment, or -1.
func openBracket(raw []byte) int {
	for i := 0; i < len(raw); {
		if j := skipToken(raw, i); j > i {
			i = j
			continue
		}
		if raw[i] == '[' {
			return i
		}
		i++
	}
	return -1
}

func depthDelta(c byte) int {
	switch c {
	case '[', '{':
		return 1
	case ']', '}':
		return -1
	}
	return 0
}

// closeEntries adds the text after the last comma as the final entry, or as the tail when it holds only comments.
func closeEntries(elems [][]byte, last []byte) (_ [][]byte, tail string) {
	if len(bytes.TrimSpace(textutil.StripJSONC(last))) > 0 {
		return append(elems, last), ""
	}
	return elems, string(bytes.TrimSpace(last))
}

// sameJSON reports whether raw (JSONC) and body decode to the same value.
func sameJSON(raw, body []byte) bool {
	var a, b any
	if json.Unmarshal(textutil.StripJSONC(raw), &a) != nil || json.Unmarshal(body, &b) != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}

// resolveModelsDest mirrors resolveKeybindingsDest: explicit wins, an explicit settings dest keeps its directory.
func resolveModelsDest(modelsDest, dest string) (path string, skip bool, note string, err error) {
	switch {
	case modelsDest != "":
		return modelsDest, false, "", nil
	case dest != "":
		return filepath.Join(filepath.Dir(dest), modelsFileName), false, "", nil
	}
	kb, skip, note, err := defaultKeybindingsDest() // chat models are UI-side too: the client's User dir
	if err != nil || skip {
		return "", skip, strings.ReplaceAll(note, "keybindings", "chat models"), err
	}
	return filepath.Join(filepath.Dir(kb), modelsFileName), false, "", nil
}
