package mcpstdio

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestInitializeNegotiatesProtocolVersion: echo a supported requested revision, else fall back to newest.
func TestInitializeNegotiatesProtocolVersion(t *testing.T) {
	for name, tc := range map[string]struct{ params, want string }{
		"echoes a supported revision":     {`{"protocolVersion":"2025-06-18"}`, "2025-06-18"},
		"echoes the newest supported":     {`{"protocolVersion":"2025-11-25"}`, "2025-11-25"},
		"echoes the oldest supported":     {`{"protocolVersion":"2024-11-05"}`, "2024-11-05"},
		"absent version falls back":       {`{}`, DefaultProtocolVersion},
		"unknown future version falls to": {`{"protocolVersion":"2099-01-01"}`, DefaultProtocolVersion},
		"garbage version falls back":      {`{"protocolVersion":"not-a-version"}`, DefaultProtocolVersion},
	} {
		t.Run(name, func(t *testing.T) {
			var out strings.Builder
			line := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":` + tc.params + "}\n"
			if err := (&Server{Name: "t"}).Serve(context.Background(), strings.NewReader(line), &out); err != nil {
				t.Fatal(err)
			}
			var resp struct {
				Result map[string]any `json:"result"`
			}
			if err := json.Unmarshal([]byte(out.String()), &resp); err != nil {
				t.Fatalf("response %q: %v", out.String(), err)
			}
			if got := resp.Result["protocolVersion"]; got != tc.want {
				t.Errorf("protocolVersion = %v, want %s", got, tc.want)
			}
		})
	}
}

// TestDefaultProtocolVersionIsHonest: never advertise 2026-07-28, which this tools-only server can't serve.
func TestDefaultProtocolVersionIsHonest(t *testing.T) {
	if DefaultProtocolVersion != "2025-11-25" || !supportedProtocolVersions[DefaultProtocolVersion] {
		t.Errorf("DefaultProtocolVersion = %s, want 2025-11-25 and supported", DefaultProtocolVersion)
	}
	if supportedProtocolVersions["2026-07-28"] {
		t.Error("must not advertise 2026-07-28: stateless core, no initialize handshake")
	}
}
