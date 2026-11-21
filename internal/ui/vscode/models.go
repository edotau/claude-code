package vscode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
	endpoints "github.com/edotau/claude-code/internal/serving"
	"github.com/edotau/claude-code/internal/textutil"
)

const (
	modelsFileName = "chatLanguageModels.json"
	groupPrefix    = "Databricks "                // the name prefix of the picker groups
	proxyKeyRef    = "${input:harnessProxyToken}" // the key reference the retired merge wrote
	maxOutputCap   = 32_000                       // the output cap current Claude/GPT tiers accept (copilot's too)
	vendorEndpoint = "customendpoint"
)

// wireAPI maps a router wire to VS Code's apiType and the full /p/<profile> path that serves it.
var wireAPI = map[string]struct{ apiType, path string }{
	providers.ProtocolAnthropic: {"messages", endpoints.ClientPath(providers.ProtocolAnthropic, "", false)},
	providers.ProtocolOpenAI:    {"chat-completions", "/" + providers.ProtocolOpenAI + endpoints.ClientPath(providers.ProtocolOpenAI, "", false)},
	providers.ProtocolResponses: {"responses", "/" + providers.ProtocolResponses + endpoints.ClientPath(providers.ProtocolResponses, "", false)},
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

// modelGroups is what --print lists: one group per profile.
type modelGroups struct {
	Groups []endpointGroup
	Base   string // the router base the groups' session URLs hang off
}

// routerBase is a seam for tests: the running proxy probe.
var routerBase = runningRouterBase

// runningRouterBase is the running proxy's base (its port may have moved), else the default port.
func runningRouterBase(ctx context.Context) string {
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	port := proxy.DefaultPort
	if st, ok := proxy.Running(probe); ok {
		port = st.Port
	}
	return proxy.ViaBaseContext(ctx, proxy.Base(port))
}

// buildDatabricksGroups lists every signed-in Databricks serving profile's cached READY chat endpoints, no network.
func buildDatabricksGroups(ctx context.Context) (modelGroups, error) {
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
		if !providers.CredentialPresent(p) { // signed out: say nothing
			continue
		}
		snap, ok := serving.RecallCtx(ctx, p)
		if !ok {
			fmt.Fprintf(os.Stderr, "claude-code: vscode models: %s has no cached listing, group skipped "+
				"(claude-code models preflight --discover %s)\n", name, name)
			continue
		}
		if base == "" {
			base = routerBase(ctx)
		}
		out.Groups = append(out.Groups, profileGroup(p, snap.Models, proxy.SessionBase(base, name)))
	}
	out.Base = base
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
	window, output := providers.Window(p, info.ID), maxOutputCap
	if l, ok := models.Limits(info.ID); ok {
		output = minPositive(output, l.Output)
	}
	claude := models.FamilyKind(info.ID) == models.FamilyClaude
	m := endpointModel{ID: info.ID, Name: info.ID, URL: session + api.path, APIType: api.apiType, ToolCalling: true,
		Vision: claude || strings.Contains(info.Capabilities, "vision"), ContextWindow: window, MaxOutputTokens: output,
		RequestHeaders: map[string]string{proxy.AgentHeader: "vscode"}}
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

// syncModels removes the groups an older merge wrote, returning their names; check never writes.
func syncModels(dest string, check bool) (mergeResult, []string, error) {
	if check {
		return mergeModels(dest, true)
	}
	var result mergeResult
	var removed []string
	err := withLock("vscode-models.lock", dest, func() (mergeErr error) {
		result, removed, mergeErr = mergeModels(dest, false)
		return mergeErr
	})
	return result, removed, err
}

// mergeModels drops every harness-owned group, keeping each other entry's original text (comments too).
func mergeModels(dest string, check bool) (mergeResult, []string, error) {
	var base []json.RawMessage
	rawBase, err := readBase(dest, "chat models", &base)
	if err != nil {
		return mergeResult{}, nil, err
	}
	head, elems, tail := splitArray(rawBase)
	if len(elems) != len(base) {
		return mergeResult{}, nil, fmt.Errorf("chat models: cannot split %d entries in the live file", len(base))
	}
	var removed []string
	items := make([][]byte, 0, len(base))
	for i, raw := range base {
		if name, ours := ownsGroup(raw); ours {
			removed = append(removed, name)
			continue
		}
		items = append(items, bytes.TrimSpace(elems[i]))
	}
	if len(removed) == 0 {
		return mergeResult{}, nil, nil
	}
	if check {
		return mergeResult{Changed: true}, removed, nil
	}
	result, err := writeGuarded(dest, "chat models", rawBase, renderArray(head, items, tail))
	return result, removed, err
}

// ownsGroup is true for a group an older merge wrote: the Databricks name, the proxy token reference and loopback URLs.
func ownsGroup(raw json.RawMessage) (name string, ours bool) {
	var g struct {
		Name, APIKey string
		Models       []struct{ URL string }
	}
	_ = json.Unmarshal(raw, &g)
	profile, prefixed := strings.CutPrefix(g.Name, groupPrefix)
	if !prefixed || g.APIKey != proxyKeyRef {
		return "", false
	}
	for _, m := range g.Models {
		if !strings.HasPrefix(m.URL, "http://127.0.0.1:") || !strings.Contains(m.URL, "/p/"+profile+"/") {
			return "", false
		}
	}
	return g.Name, true
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

// resolveModelsDest mirrors resolveKeybindingsDest: explicit wins, an explicit settings dest keeps its directory.
func resolveModelsDest(modelsDest, dest string) (path string, skip bool, note string, err error) {
	switch {
	case modelsDest != "":
		return modelsDest, false, "", nil
	case dest != "":
		return filepath.Join(filepath.Dir(dest), modelsFileName), false, "", nil
	}
	kb, skip, note, err := defaultKeybindingsDest()
	if err != nil || skip {
		return "", skip, strings.ReplaceAll(note, "keybindings", "chat models"), err
	}
	return filepath.Join(filepath.Dir(kb), modelsFileName), false, "", nil
}

// printModels writes the Databricks chat models as JSON for the editor's chat provider; it never writes a file.
func printModels(w io.Writer) error {
	mg, err := buildDatabricksGroups(context.Background())
	if err != nil {
		return fmt.Errorf("models: %w", err)
	}
	type model struct {
		ID              string `json:"id"`
		ToolCalling     bool   `json:"toolCalling"`
		Vision          bool   `json:"vision"`
		ContextWindow   int    `json:"contextWindow"`
		MaxOutputTokens int    `json:"maxOutputTokens"`
	}
	type profile struct {
		Profile string  `json:"profile"`
		Base    string  `json:"base"`
		Models  []model `json:"models"`
	}
	out := struct {
		Profiles []profile `json:"profiles"`
	}{Profiles: []profile{}}
	for _, g := range mg.Groups {
		name := strings.TrimPrefix(g.Name, groupPrefix)
		pr := profile{Profile: name, Base: proxy.SessionBase(mg.Base, name), Models: []model{}}
		for _, m := range g.Models {
			pr.Models = append(pr.Models, model{m.ID, m.ToolCalling, m.Vision, m.ContextWindow, m.MaxOutputTokens})
		}
		out.Profiles = append(out.Profiles, pr)
	}
	return json.NewEncoder(w).Encode(out)
}
