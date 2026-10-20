package vault

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/keyring"
	"github.com/edotau/claude-code/internal/paths"
)

var keyringLookup = keyring.Lookup
var keyringSet = keyring.Set
var keyringDelete = keyring.Delete

// UseKeyringSeams swaps the lookup/set/delete functions this package shells out to for the OS
// keyring, returning a restore func; exported solely for vaulttest's FakeKeyring.
func UseKeyringSeams(lookup func(string) (string, bool, error), set func(string, string) error, del func(string) error) (restore func()) {
	savedLookup, savedSet, savedDelete := keyringLookup, keyringSet, keyringDelete
	keyringLookup, keyringSet, keyringDelete = lookup, set, del
	return func() { keyringLookup, keyringSet, keyringDelete = savedLookup, savedSet, savedDelete }
}

// testHookAfterKey runs right after UpdateSealed selects a key and before it opens the sealing
// tx; a spec test uses it to pause a writer mid-rotation (TestStaleWriterRetries).
var testHookAfterKey = func() {}

// errStaleKey means the tx's meta/kid no longer matches the key UpdateSealed selected before
// opening it — a rotation landed between selection and commit; the caller refreshes and retries.
var errStaleKey = errors.New("vault: master key is stale")

// account is the keyring account for a vault's current master key; account+".next" is a pending
// rotation, account+"."+kid is an immutable retired key (R1, R4).
func account(id string) string { return "vault-key/" + id }

var keyCache struct {
	sync.Mutex
	path, vaultID, kid, command, source string
	key                                 *Key
}

// invalidateKey clears the per-process key cache so the next MasterKey call re-resolves from the
// keyring/env/key_command instead of serving a key a concurrent rotation has since retired.
func invalidateKey() {
	keyCache.Lock()
	defer keyCache.Unlock()
	keyCache.key = nil
}

func keyMeta() (id, kid, command string, err error) {
	err = View(func(t *Tx) error {
		id = vaultID(t)
		kid, _ = t.Get(Meta, "kid")
		command, _ = t.Get(Meta, "key_command")
		return nil
	})
	return
}

func MasterKey() (*Key, string, error) {
	id, kid, command, err := keyMeta()
	if err != nil {
		return nil, "", err
	}
	return resolveKey(id, kid, command)
}

// resolveKey resolves the key expected to carry kid for vault id, trying HARNESS_VAULT_KEY, the
// per-process cache, the keyring account, then key_command — the shared core MasterKey and
// ReadSecret both use, parameterized so a reader can anchor on a kid it captured earlier (R3).
func resolveKey(id, kid, command string) (*Key, string, error) {
	if v := os.Getenv("HARNESS_VAULT_KEY"); v != "" {
		k, err := parseKey(v)
		if err != nil {
			return nil, "env", err
		}
		return matchKey(id, k, "env", kid)
	}
	keyCache.Lock()
	defer keyCache.Unlock()
	if keyCache.key != nil && keyCache.path == Path() && keyCache.vaultID == id &&
		keyCache.kid == kid && keyCache.command == command {
		return keyCache.key, keyCache.source, nil
	}
	v, found, lookupErr := keyringLookup(account(id))
	var k *Key
	var err error
	source := "keyring"
	switch {
	case lookupErr != nil:
		return nil, "", lookupErr
	case found:
		k, err = parseKey(v)
	case command != "":
		source = "command"
		k, err = runKeyCommand(command)
	default:
		return nil, "", ErrLocked
	}
	if err != nil {
		return nil, source, err
	}
	k, source, err = matchKey(id, k, source, kid)
	if err != nil {
		return nil, source, err
	}
	keyCache.path, keyCache.vaultID, keyCache.kid, keyCache.command, keyCache.source, keyCache.key =
		Path(), id, kid, command, source, k
	return k, source, nil
}

// runKeyCommand runs command as argv (no shell) and parses its stdout as a master key.
func runKeyCommand(command string) (*Key, error) {
	argv := strings.Fields(command)
	if len(argv) == 0 {
		return nil, fmt.Errorf("vault: key_command is empty")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = 250 * time.Millisecond
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("vault: key_command: %w", ctx.Err())
	}
	if err != nil {
		return nil, err
	}
	return parseKey(string(out))
}

// matchKey resolves k against the vault's recorded kid: a direct hit returns k unchanged; a
// mismatch tries the retired account for kid, then a pending rotation, before giving up.
func matchKey(id string, k *Key, source, kid string) (*Key, string, error) {
	if k != nil && (kid == "" || k.Kid == kid) {
		return k, source, nil
	}
	if kid == "" {
		return nil, "", ErrLocked
	}
	if v, found, err := keyringLookup(account(id) + "." + kid); err != nil {
		return nil, source, err
	} else if found {
		if rk, perr := parseKey(v); perr == nil && rk.Kid == kid {
			return rk, "keyring", nil
		}
	}
	v, found, err := keyringLookup(account(id) + ".next")
	if err != nil {
		return nil, source, err
	}
	if found {
		next, perr := parseKey(v)
		if perr != nil {
			return nil, source, perr
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

// wrapKeyringUnavailable turns a keyring that could not positively establish absence into a
// locked vault with recovery instructions, instead of letting EnsureKey generate over it (R6).
func wrapKeyringUnavailable(err error) error {
	if errors.Is(err, keyring.ErrUnavailable) || errors.Is(err, keyring.ErrNoAnswer) {
		return fmt.Errorf("%w: set HARNESS_VAULT_KEY or meta/key_command (%v)", ErrLocked, err)
	}
	return err
}

func EnsureKey() (*Key, error) {
	k, _, err := MasterKey()
	if err == nil {
		return k, nil
	}
	if !errors.Is(err, ErrLocked) {
		return nil, wrapKeyringUnavailable(err)
	}
	var result *Key
	err = paths.WithFileLock(Path()+".key", func() error {
		k, _, err := MasterKey()
		if err == nil {
			result = k
			return nil
		}
		if !errors.Is(err, ErrLocked) {
			return wrapKeyringUnavailable(err)
		}
		id, err := ensureVaultID()
		if err != nil {
			return err
		}
		_, kid, _, err := keyMeta()
		if err != nil {
			return err
		}
		if kid != "" {
			// A kid is recorded but no source above could open it: locked, not absent.
			return ErrLocked
		}
		_, found, lookupErr := keyringLookup(account(id))
		if lookupErr != nil {
			return wrapKeyringUnavailable(lookupErr)
		}
		if found {
			// A key is already in the keyring; meta/kid being empty here would be a corrupt
			// state, not an invitation to generate a second one.
			return ErrLocked
		}
		newK, value, err := randomKey()
		if err != nil {
			return err
		}
		if err := keyringSet(account(id), value); err != nil {
			return err
		}
		if err := storeKeyID(newK); err != nil {
			return err
		}
		result = newK
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ensureVaultID materializes the vault file (and its random vault_id) if neither exists yet, and
// returns the id; Update backfills vault_id on any pre-existing vault that predates it.
func ensureVaultID() (string, error) {
	var id string
	err := Update(func(t *Tx) error {
		id = vaultID(t)
		return nil
	})
	return id, err
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

// RotateKey generates a new master key, re-seals every secret under it, and retains the outgoing
// key forever under its own immutable kid account so an archive or a stalled writer can still
// open records sealed under it (R2, R4).
func RotateKey() (oldKid, newKid string, err error) {
	err = paths.WithFileLock(Path()+".key", func() error {
		defer invalidateKey()
		old, source, e := MasterKey()
		if e != nil {
			return e
		}
		if source != "keyring" || os.Getenv("HARNESS_VAULT_KEY") != "" {
			return fmt.Errorf("vault: rotation requires a keyring-held master key")
		}
		oldKid = old.Kid
		id, kid, _, e := keyMeta()
		if e != nil {
			return e
		}
		acct := account(id)

		// Recover a rotation that committed its re-seal but crashed before promotion: nothing
		// sealed under the stale primary account any more, so just finish the handoff.
		pending, found, e := keyringLookup(acct + ".next")
		if e != nil {
			return e
		}
		if found {
			next, e := parseKey(pending)
			if e != nil {
				return e
			}
			if next.Kid == kid {
				if current, found, e := keyringLookup(acct); e != nil {
					return e
				} else if found {
					if primary, e := parseKey(current); e == nil {
						oldKid = primary.Kid
					}
				}
				newKid = next.Kid
				if e := keyringSet(acct, pending); e != nil {
					return e
				}
				if e := keyringDelete(acct + ".next"); e != nil {
					return e
				}
				audit.Log(audit.KeyRotate, oldKid+"->"+newKid, "")
				return nil
			}
			// A foreign/stale pending key: the tx that would use it also sets meta/kid
			// atomically, so nothing is sealed under it — safe to discard and continue.
			if e := keyringDelete(acct + ".next"); e != nil {
				return e
			}
		}

		next, value, e := randomKey()
		if e != nil {
			return e
		}
		newKid = next.Kid
		// Retain the outgoing key under its immutable kid account before anything else changes.
		if e := keyringSet(acct+"."+old.Kid, old.raw); e != nil {
			return e
		}
		if e := keyringSet(acct+".next", value); e != nil {
			return e
		}
		if e := Update(func(t *Tx) error {
			curKid, _ := t.Get(Meta, "kid")
			if curKid != "" && curKid != old.Kid {
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
				if e := t.PutRecord(name, r); e != nil {
					return e
				}
			}
			return t.Put(Meta, "kid", next.Kid)
		}); e != nil {
			return e
		}
		if e := keyringSet(acct, value); e != nil {
			return e
		}
		if e := keyringDelete(acct + ".next"); e != nil {
			return e
		}
		audit.Log(audit.KeyRotate, oldKid+"->"+newKid, "")
		return nil
	})
	return
}

// UpdateSealed selects a key outside the tx and verifies meta/kid inside it; a stale key (a
// rotation landed between selection and commit) is refreshed and retried, bounded at 3 attempts.
func UpdateSealed(fn func(*Tx, *Key) error) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		var k *Key
		k, err = EnsureKey()
		if err != nil {
			return err
		}
		testHookAfterKey()
		err = Update(func(t *Tx) error {
			kid, ok := t.Get(Meta, "kid")
			if !ok || kid == "" {
				if e := t.Put(Meta, "kid", k.Kid); e != nil {
					return e
				}
			} else if kid != k.Kid {
				return errStaleKey
			}
			return fn(t, k)
		})
		if errors.Is(err, errStaleKey) {
			invalidateKey()
			continue
		}
		return err
	}
	if errors.Is(err, errStaleKey) {
		return ErrWrongKey
	}
	return err
}

// ReadSecret reads one record and its kid in a single coherent View, then opens it with the key
// for that exact kid, retrying once if a concurrent rotation invalidated the key mid-read (R3).
func ReadSecret(name string) (value string, found bool, err error) {
	for attempt := 0; attempt < 2; attempt++ {
		var id, kid, command string
		var record Record
		var ok bool
		if verr := View(func(t *Tx) error {
			id = vaultID(t)
			kid, _ = t.Get(Meta, "kid")
			command, _ = t.Get(Meta, "key_command")
			record, ok = t.Record(name)
			return nil
		}); verr != nil {
			return "", false, verr
		}
		if !ok {
			return "", false, nil
		}
		k, _, kerr := resolveKey(id, kid, command)
		if kerr != nil {
			if errors.Is(kerr, ErrWrongKey) && attempt == 0 {
				invalidateKey()
				continue
			}
			return "", false, kerr
		}
		v, oerr := k.Open(name, record)
		if oerr != nil {
			if errors.Is(oerr, ErrWrongKey) && attempt == 0 {
				invalidateKey()
				continue
			}
			return "", false, oerr
		}
		return v, true, nil
	}
	return "", false, ErrWrongKey
}
