// antigravity.go — live Google quota via agy's `/usage` headless command (Antigravity's own CLI).
package usage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os/exec"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// agyUsageCmd is the agy exec seam; tests replace it with a fake binary so none of this reaches agy.
var agyUsageCmd = func(ctx context.Context) ([]byte, error) {
	bin, err := paths.VendorBin("agy")
	if err != nil {
		return nil, err
	}
	return exec.CommandContext(ctx, bin, "-p", "/usage", "--output-format", "json", "--print-timeout", "20s").Output()
}

// agyUsageBucket is one rate-limit bucket inside an agy usage group; disabled buckets carry no numbers.
type agyUsageBucket struct {
	Window        string  `json:"window"`
	RemainingFrac float64 `json:"remaining_fraction"`
	ResetTime     string  `json:"reset_time"`
	Disabled      bool    `json:"disabled"`
}

// agyUsageGroup is one named group of buckets ("Gemini Models", "Claude and GPT models", ...).
type agyUsageGroup struct {
	Name    string           `json:"name"`
	Buckets []agyUsageBucket `json:"buckets"`
}

// agyUsageResult mirrors agy -p "/usage" --output-format json: one top-level status plus the usage command.
type agyUsageResult struct {
	Status  string `json:"status"`
	Command struct {
		Name string `json:"name"`
		Data struct {
			Groups []agyUsageGroup `json:"groups"`
		} `json:"data"`
	} `json:"command"`
}

// antigravityQuotas reads agy's `/usage` output: one quota row per enabled bucket, "google"/"Antigravity".
func antigravityQuotas(ctx context.Context) []quota {
	out, err := agyUsageCmd(ctx)
	if err != nil && len(out) == 0 {
		if _, lookErr := paths.VendorBin("agy"); lookErr != nil {
			return agyNote("agy not installed")
		}
		return agyNote("agy usage failed: " + firstLine(out, err))
	}
	if strings.Contains(string(out), "not logged in") || strings.Contains(string(out), "run agy login") {
		return agyNote("agy not logged in (run agy login)")
	}
	if i := strings.Index(string(out), "AGY_ERROR:"); i >= 0 {
		return agyNote("agy usage failed: " + firstLine(out[i:], err))
	}
	res, findErr := findAgyUsageResult(out)
	if findErr != nil {
		return agyNote("agy usage failed: " + findErr.Error())
	}
	return agyUsageRows(res)
}

// agyNote is the single google row shown when agy can't report numbers.
func agyNote(note string) []quota {
	return []quota{{Provider: "google", Plan: "Antigravity", Note: note}}
}

// agyUsageRows turns every enabled bucket into one quota row, detail carrying the bucket's group label.
func agyUsageRows(res agyUsageResult) []quota {
	var out []quota
	for _, g := range res.Command.Data.Groups {
		for _, b := range g.Buckets {
			if b.Disabled {
				continue
			}
			out = append(out, quota{
				Provider: "google", Plan: "Antigravity", Account: "-",
				Window:   b.Window,
				UsedPct:  math.Round((1 - b.RemainingFrac) * 100),
				Resets:   formatAgyReset(b.ResetTime),
				ResetsAt: agyResetUTC(b.ResetTime),
				Group:    g.Name,
			})
		}
	}
	if len(out) == 0 {
		return agyNote("agy usage reported no buckets")
	}
	return out
}

// findAgyUsageResult scans stdout line by line for the first JSON object naming the usage command.
func findAgyUsageResult(out []byte) (agyUsageResult, error) {
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 0, 64*1024), 512*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var res agyUsageResult
		if err := json.Unmarshal(line, &res); err != nil {
			continue
		}
		if res.Status == "SUCCESS" && res.Command.Name == "usage" {
			return res, nil
		}
	}
	return agyUsageResult{}, errors.New("no successful usage result in agy output")
}

// firstLine trims a command's combined error to one line, capped at 80 chars, never the whole stream.
func firstLine(out []byte, err error) string {
	s := strings.TrimSpace(string(out))
	if s == "" {
		s = err.Error()
	}
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

// formatAgyReset parses agy's RFC3339 reset_time and renders it local, matching the openai rows' format.
func formatAgyReset(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return ""
	}
	return t.Local().Format("2006-01-02 15:04")
}

// agyResetUTC parses agy's RFC3339 reset_time and normalizes it to a UTC instant for ResetsAt.
func agyResetUTC(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
