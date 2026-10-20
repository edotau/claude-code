package vault

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/keyring"
	"github.com/edotau/claude-code/internal/paths"
)

var keyringLookup = keyring.Lookup
var keyringSet = keyring.Set
var keyringDelete = keyring.Delete

var keyCache struct {
	sync.Mutex
	path, kid, command, source string
	key                        *Key
}

func keyMeta() (kid, command string, err error) {
	err = View(func(t *Tx) error {
		kid, _ = t.Get(Meta, "kid")
		command, _ = t.Get(Meta, "key_command")
		return nil
	})
	return
}

func MasterKey() (*Key, string, error) {
	kid, command, err := keyMeta()
	if err != nil {
		return nil, "", err
	}
	if v := os.Getenv("HARNESS_VAULT_KEY"); v != "" {
		k, err := parseKey(v)
		if err != nil {
			return nil, "env", err
		}
		return matchKey(k, "env", kid)
	}
	keyCache.Lock()
	defer keyCache.Unlock()
	if keyCache.key != nil && keyCache.path == Path() && keyCache.kid == kid && keyCache.command == command {
		return keyCache.key, keyCache.source, nil
	}
	v, found, lookupErr := keyringLookup("vault-key")
	var k *Key
	source := "keyring"
	if found {
		k, err = parseKey(v)
	} else if command != "" {
		source = "command"
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", "-c", command)
		cmd.WaitDelay = 250 * time.Millisecond
		var out []byte
		out, err = cmd.Output()
		if ctx.Err() != nil {
			return nil, source, fmt.Errorf("vault: key_command: %w", ctx.Err())
		}
		if err == nil {
			k, err = parseKey(string(out))
		}
	} else if lookupErr != nil {
		return nil, "", lookupErr
	}
	if err != nil {
		return nil, source, err
	}
	k, source, err = matchKey(k, source, kid)
	if err != nil {
		return nil, source, err
	}
	keyCache.path, keyCache.kid, keyCache.command, keyCache.source, keyCache.key = Path(), kid, command, source, k
	return k, source, nil
}

func matchKey(k *Key, source, kid string) (*Key, string, error) {
	if k != nil && (kid == "" || k.Kid == kid) {
		return k, source, nil
	}
	if kid == "" {
		return nil, "", ErrLocked
	}
	v, found, err := keyringLookup("vault-key.next")
	if err != nil {
		return nil, source, err
	}
	if found {
		next, err := parseKey(v)
		if err != nil {
			return nil, source, err
		}
		if next.Kid == kid {
			return next, "keyring", nil
		}
	}
	if k == nil {
		return nil, "", ErrLocked
	}
	return nil, source, ErrWrongKey
}

func EnsureKey() (*Key, error) {
	k, _, err := MasterKey()
	if err == nil {
		kid, _, metaErr := keyMeta()
		if metaErr != nil {
			return nil, metaErr
		}
		if kid != "" {
			return k, nil
		}
	} else if !errors.Is(err, ErrLocked) {
		return nil, err
	}
	err = paths.WithFileLock(Path()+".key", func() error {
		k, _, err = MasterKey()
		if err == nil {
			return storeKeyID(k)
		}
		if !errors.Is(err, ErrLocked) {
			return err
		}
		kid, _, err := keyMeta()
		if err != nil {
			return err
		}
		if kid != "" {
			return ErrLocked
		}
		_, found, err := keyringLookup("vault-key")
		if err != nil {
			return err
		}
		if found {
			return ErrLocked
		}
		var value string
		k, value, err = randomKey()
		if err != nil {
			return err
		}
		if err = keyringSet("vault-key", value); err != nil {
			return err
		}
		return storeKeyID(k)
	})
	if err != nil {
		return nil, err
	}
	return k, nil
}

func storeKeyID(k *Key) error {
	return Update(func(t *Tx) error {
		kid, _ := t.Get(Meta, "kid")
		if kid != "" && kid != k.Kid {
			return ErrWrongKey
		}
		return t.Put(Meta, "kid", k.Kid)
	})
}

func randomKey() (*Key, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, "", err
	}
	value := base64.StdEncoding.EncodeToString(raw)
	k, err := parseKey(value)
	return k, value, err
}

func RotateKey() (oldKid, newKid string, err error) {
	err = paths.WithFileLock(Path()+".key", func() error {
		old, source, e := MasterKey()
		if e != nil {
			return e
		}
		if source != "keyring" || os.Getenv("HARNESS_VAULT_KEY") != "" {
			return fmt.Errorf("vault: rotation requires a keyring-held master key")
		}
		oldKid = old.Kid
		kid, _, e := keyMeta()
		if e != nil {
			return e
		}
		pending, found, e := keyringLookup("vault-key.next")
		if e != nil {
			return e
		}
		if found {
			next, e := parseKey(pending)
			if e != nil {
				return e
			}
			// Promote a committed rotation before replacing its only recoverable key.
			if next.Kid == kid {
				if current, found, e := keyringLookup("vault-key"); e != nil {
					return e
				} else if found {
					primary, e := parseKey(current)
					if e != nil {
						return e
					}
					oldKid = primary.Kid
				}
				newKid = next.Kid
				if e = keyringSet("vault-key", pending); e != nil {
					return e
				}
				return keyringDelete("vault-key.next")
			}
		}
		next, value, e := randomKey()
		if e != nil {
			return e
		}
		newKid = next.Kid
		if e = keyringSet("vault-key.next", value); e != nil {
			return e
		}
		if e = Update(func(t *Tx) error {
			kid, _ := t.Get(Meta, "kid")
			if kid != "" && kid != old.Kid {
				return ErrWrongKey
			}
			for _, name := range t.Names(Secrets) {
				r, ok := t.Record(name)
				if !ok {
					return fmt.Errorf("vault: invalid record %q", name)
				}
				plain, e := old.Open(name, r)
				if e != nil {
					return e
				}
				r.Sealed = next.Seal(name, plain)
				r.Kid = next.Kid
				if e = t.PutRecord(name, r); e != nil {
					return e
				}
			}
			return t.Put(Meta, "kid", next.Kid)
		}); e != nil {
			return e
		}
		if e = keyringSet("vault-key", value); e != nil {
			return e
		}
		return keyringDelete("vault-key.next")
	})
	return
}
