package vault

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
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

// account is the keyring account for a vault's current master key; account+"."+kid is the
// immutable, never-deleted account every key — current or retired — is durably stored under,
// written BEFORE any ciphertext is committed under that kid (R1, R2, R4). account alone is a
// convenience pointer to whichever kid is current: resolveKey/matchKey fall back to the per-kid
// account whenever the pointer is stale, absent, or shared by a copied root mid-rotation.
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
//
// An explicitly configured key_command is tried whenever the keyring could not positively
// establish absence (ErrUnavailable/ErrNoAnswer — a locked session, no tool, Windows), not only on
// a clean miss: the keyring plane going dark must not strand the documented recovery path (R3/#3).
// A genuine keyring error with no usable alternative still surfaces as ErrLocked with the
// recovery instructions, never the raw keyring error.
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
	recoverable := lookupErr != nil && (errors.Is(lookupErr, keyring.ErrUnavailable) || errors.Is(lookupErr, keyring.ErrNoAnswer))
	if lookupErr != nil && !recoverable {
		return nil, "", lookupErr
	}
	var k *Key
	var err error
	source := "keyring"
	switch {
	case found:
		k, err = parseKey(v)
	case command != "":
		source = "command"
		k, err = runKeyCommand(command)
	case recoverable:
		return nil, "", wrapKeyringUnavailable(lookupErr)
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

// parseKeyCommandArgv decodes meta/key_command as a validated JSON argv array: no shell is ever
// involved, so a quoted executable path or an argument containing spaces or quotes needs no
// quoting grammar of our own (R6).
func parseKeyCommandArgv(raw string) ([]string, error) {
	var argv []string
	if err := json.Unmarshal([]byte(raw), &argv); err != nil {
		return nil, fmt.Errorf("vault: key_command must be a JSON array of strings: %w", err)
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("vault: key_command must not be an empty argv array")
	}
	for i, a := range argv {
		if a == "" {
			return nil, fmt.Errorf("vault: key_command argv[%d] must not be empty", i)
		}
	}
	return argv, nil
}

// runKeyCommand runs raw's decoded argv directly (no shell) and parses its stdout as a master key.
func runKeyCommand(raw string) (*Key, error) {
	argv, err := parseKeyCommandArgv(raw)
	if err != nil {
		return nil, err
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
// mismatch falls back to the immutable per-kid account, which every key — current or retired —
// is durably stored under regardless of what the convenience pointer currently says (R1, R2).
func matchKey(id string, k *Key, source, kid string) (*Key, string, error) {
	if k != nil && (kid == "" || k.Kid == kid) {
		return k, source, nil
	}
	if kid == "" {
		return nil, "", ErrLocked
	}
	v, found, err := keyringLookup(account(id) + "." + kid)
	if err != nil {
		return nil, source, err
	}
	if found {
		if rk, perr := parseKey(v); perr == nil && rk.Kid == kid {
			return rk, "keyring", nil
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
		// Persist the genesis key under its immutable per-kid account before the convenience
		// pointer, so matchKey can always find it even if a copied root's pointer diverges later.
		if err := keyringSet(account(id)+"."+newK.Kid, value); err != nil {
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

// RotateKey generates a new master key, re-seals every secret under it, and retains every key —
// outgoing and incoming — forever under its own immutable kid account so an archive, a stalled
// writer, or a sibling root sharing this vault_id mid-rotation can still open records sealed
// under it (R2, R4).
//
// Design (R1, R2): the new key is written to its immutable account+"."+newKid account BEFORE the
// re-seal transaction runs, and the convenience current-key pointer (account(id)) is updated only
// AFTER that transaction commits. Recovery needs no separate "pending" bookkeeping: whatever the
// pointer says, resolveKey/matchKey always find the key matching the committed meta/kid under its
// own immutable account, so a crash at any point between those two writes — or a second root's
// rotation overwriting the shared pointer with an unrelated kid — strands nothing. The only
// outcome of an interrupted rotation is a harmless, never-read orphan key sitting under its own
// kid account.
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
		if kid != "" && kid != old.Kid {
			return ErrWrongKey
		}
		acct := account(id)

		// Retain the outgoing key under its immutable kid account; idempotent if an earlier
		// rotation (or this one, resumed after a crash) already wrote it.
		if e := keyringSet(acct+"."+old.Kid, old.raw); e != nil {
			return e
		}

		next, value, e := randomKey()
		if e != nil {
			return e
		}
		newKid = next.Kid

		// Persist the new key under its immutable kid account BEFORE any ciphertext is committed
		// under it (R1): a crash after this line but before (or during) the reseal below leaves
		// the new key durable and unreferenced — harmless — never stranded or lost.
		if e := keyringSet(acct+"."+newKid, value); e != nil {
			return e
		}

		if e := Update(func(t *Tx) error {
			curKid, _ := t.Get(Meta, "kid")
			if curKid != "" && curKid != old.Kid {
				return ErrWrongKey
			}
			for _, name := range t.Names(Secrets) {
				r, ok, rerr := t.Record(name)
				if rerr != nil {
					return rerr
				}
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
		// Convenience pointer only, updated after the atomic reseal commits; resolveKey/matchKey
		// never depend on this being current (R1, R2).
		if e := keyringSet(acct, value); e != nil {
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
			var rerr error
			record, ok, rerr = t.Record(name)
			return rerr
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
