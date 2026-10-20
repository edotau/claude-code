package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/secrets"
)

func legacyDir() string { return filepath.Join(paths.ConfigDir(), "env.d") }

func rootProvidersFile() string { return filepath.Join(paths.ConfigDir(), "providers.json") }

type legacySource struct {
	path string
	kind string
}

func legacySources() []legacySource {
	sources := []legacySource{
		{filepath.Join(legacyDir(), "provider.env"), "pins"},
		{filepath.Join(legacyDir(), "custom.env"), "env"},
		{filepath.Join(legacyDir(), "providers.json"), "providers"},
		{filepath.Join(legacyDir(), "vendors.json"), "vendors"},
		{filepath.Join(legacyDir(), "secrets.env"), "secrets"},
		{filepath.Join(legacyDir(), "secrets.meta.json"), "secrets-meta"},
		{rootProvidersFile(), "providers"},
	}
	var found []legacySource
	for _, source := range sources {
		if abs, err := filepath.Abs(source.path); err == nil {
			source.path = abs
		}
		if _, err := os.Lstat(source.path); !errors.Is(err, fs.ErrNotExist) {
			found = append(found, source)
		}
	}
	return found
}

// LegacyFiles lists existing migration sources, never backups or lock files.
func LegacyFiles() []string {
	var files []string
	for _, source := range legacySources() {
		files = append(files, source.path)
	}
	return files
}

func legacyLocks() []string {
	var locks []string
	for _, name := range []string{
		"custom.env", "provider.env", "providers.json", "secrets.env", "secrets.meta.json",
	} {
		locks = append(locks, filepath.Join(legacyDir(), name))
	}
	return locks
}

func legacyLabel(path string) string {
	if rel, err := filepath.Rel(paths.ConfigDir(), path); err == nil {
		return filepath.ToSlash(rel)
	}
	return path
}

func duplicateProvidersError() error {
	return fmt.Errorf("providers.json exists in both %s and env.d; merge, then re-run", paths.ConfigDir())
}

// parseEnvFile reads a legacy env file through the one strict dotenv parser; any problem fails the file (R5).
func parseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	pairs, problems := secrets.ParseDotenv(f)
	if len(problems) > 0 {
		return nil, fmt.Errorf("%s: %s", path, strings.Join(problems, "; "))
	}
	values := make(map[string]string, len(pairs))
	for _, p := range pairs {
		values[p.Name] = p.Value
	}
	return values, nil
}

type legacyMetadata struct {
	Version     int                         `json:"version"`
	Credentials map[string]legacyCredential `json:"credentials"`
}

type legacyCredential struct {
	Plane        string    `json:"plane"`
	Prefix       string    `json:"prefix"`
	Len          int       `json:"len"`
	SetAt        time.Time `json:"set_at"`
	VerifiedAt   time.Time `json:"verified_at"`
	VerifyStatus string    `json:"verify_status"`
}
