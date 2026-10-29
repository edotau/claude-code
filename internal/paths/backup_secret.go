package paths

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// WriteBackup writes data to base, or base-1, base-2... if taken, never overwriting; returns the path used.
func WriteBackup(base string, data []byte) (string, error) {
	for i := 0; ; i++ {
		dest := base
		if i > 0 {
			dest = fmt.Sprintf("%s-%d", base, i)
		}
		f, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, err = f.Write(data)
		if err = errors.Join(err, f.Close()); err != nil {
			return "", errors.Join(err, os.Remove(dest))
		}
		return dest, nil
	}
}

// Backup writes data to the cache-mirrored backup path for src (dir made 0700), never overwriting; returns the path.
func Backup(src string, data []byte) (string, error) {
	base := BackupPath(src, time.Now())
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		return "", fmt.Errorf("backup dir %s: %w", filepath.Dir(base), err)
	}
	return WriteBackup(base, data)
}

// ClientSecret returns the 32-byte hex secret in file, minting it 0600 under a file lock on first use.
func ClientSecret(file string) (string, error) {
	if s, err := readSecret(file); err == nil {
		return s, nil
	}
	var out string
	err := WithFileLock(file, func() error {
		if s, err := readSecret(file); err == nil {
			out = s
			return nil
		}
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return err
		}
		out = hex.EncodeToString(b)
		return AtomicWrite(file, []byte(out+"\n"), 0o600)
	})
	return out, err
}

func readSecret(file string) (string, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	if s := strings.TrimSpace(string(b)); s != "" {
		return s, nil
	}
	return "", errors.New("empty secret file " + file)
}
