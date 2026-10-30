package vendors

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

var catalogWarn io.Writer = os.Stderr

// SetCatalogWarn redirects catalog warnings and returns the previous writer; tests use it to keep stderr quiet.
func SetCatalogWarn(w io.Writer) io.Writer { old := catalogWarn; catalogWarn = w; return old }

// menuWire spells a bare menu id the way p serves it; an id outside the catalog still takes the row's prefix once.
func menuWire(p *providers.Provider, id string) string {
	if w := p.WireModel(id); w != id {
		return w
	}
	rule := p.SpellingRule()
	if _, _, known := models.Resolve(id); known || rule == "" ||
		strings.HasPrefix(id, models.DatabricksPrefix) || strings.HasPrefix(id, "system.ai.") {
		return id
	}
	return models.Wire(rule, id)
}

// codexSlug is codex's own built-in slug for a menu id: the dotted spelling (gpt-6.1-sol); an unknown id stays as typed.
func codexSlug(id string) string {
	if canonical, _, ok := models.Resolve(id); ok {
		return models.Wire("dotted", canonical)
	}
	return id
}

// codexCatalogArgs returns a validated Databricks model catalog override, or lets launch continue without one.
func codexCatalogArgs(p *providers.Provider) []string {
	if p.Preset != providers.PresetDatabricks {
		return nil
	}
	path, missing, err := codexCatalogPath(p)
	if err != nil {
		fmt.Fprintln(catalogWarn, "codex catalog:", err)
		return nil
	}
	if missing != "" {
		fmt.Fprintln(catalogWarn, "codex is missing metadata for "+missing+"; update codex")
	}
	if path == "" {
		return nil
	}
	return []string{"-c", "model_catalog_json=" + TOMLQuote(path)}
}

func codexCatalogPath(p *providers.Provider) (string, string, error) {
	bin, err := paths.VendorBin("codex")
	if err != nil {
		return "", "", fmt.Errorf("binary lookup failed: %w", err)
	}
	home, err := os.MkdirTemp("", "codex-catalog-")
	if err != nil {
		return "", "", fmt.Errorf("temporary home failed: %w", err)
	}
	defer func() { _ = os.RemoveAll(home) }()
	version, err := runCodexCatalog(bin, home, "--version")
	if err != nil {
		return "", "", fmt.Errorf("version failed: %w", err)
	}
	menu := providers.OpencodeMenu()
	parts := []string{string(version), p.Name}
	for _, m := range menu {
		parts = append(parts, menuWire(p, m.ID), m.Label, strconv.FormatBool(m.Responses))
	}
	rawKey, err := json.Marshal(parts)
	if err != nil {
		return "", "", fmt.Errorf("cache key failed: %w", err)
	}
	key := fmt.Sprintf("%x", sha256.Sum256(rawKey))
	dir := filepath.Join(paths.StateDir(), "codex")
	path := filepath.Join(dir, "catalog-"+p.Name+".json")
	keyPath := filepath.Join(dir, "catalog-"+p.Name+".key")
	if cached, readErr := os.ReadFile(keyPath); readErr == nil && string(cached) == key {
		if info, statErr := os.Stat(path); statErr == nil && info.Mode().IsRegular() {
			return path, "", nil
		}
	}
	raw, err := runCodexCatalog(bin, home, "debug", "models", "--bundled")
	if err != nil {
		return "", "", fmt.Errorf("bundled dump failed: %w", err)
	}
	body, missing, err := buildCodexCatalog(p, menu, raw)
	if err != nil {
		return "", "", fmt.Errorf("bundled metadata failed: %w", err)
	}
	if body == nil {
		return "", missing, nil
	}
	candidate := filepath.Join(home, "catalog.json")
	if err := os.WriteFile(candidate, body, 0o600); err != nil {
		return "", "", fmt.Errorf("candidate write failed: %w", err)
	}
	if _, err := runCodexCatalog(bin, home, "-c", "model_catalog_json="+TOMLQuote(candidate), "debug", "models"); err != nil {
		return "", "", fmt.Errorf("validation failed: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", fmt.Errorf("directory creation failed: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", "", fmt.Errorf("directory permissions failed: %w", err)
	}
	if err := os.Remove(keyPath); err != nil && !os.IsNotExist(err) {
		return "", "", fmt.Errorf("cache key invalidation failed: %w", err)
	}
	if err := writeCodexCatalog(path, body); err != nil {
		return "", "", fmt.Errorf("catalog write failed: %w", err)
	}
	if err := writeCodexCatalog(keyPath, []byte(key)); err != nil {
		return "", "", fmt.Errorf("cache key write failed: %w", err)
	}
	return path, missing, nil
}

func runCodexCatalog(bin, home string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), "CODEX_HOME="+home)
	cmd.Dir = home
	return cmd.Output()
}

func writeCodexCatalog(path string, body []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".catalog-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
