package vault

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/paths"
)

var keyringLookup = KeyringLookup
var keyringSet = osKeyringSet

// UseKeyringSeams swaps the OS keyring lookup/set seams and returns a restore func; only vaulttest calls it.
func UseKeyringSeams(lookup func(string) (string, bool, error), set func(string, string) error) (restore func()) {
	savedLookup, savedSet := keyringLookup, keyringSet
	keyringLookup, keyringSet = lookup, set
	return func() { keyringLookup, keyringSet = savedLookup, savedSet }
}

// testHookAfterKey runs between UpdateSealed's key pick and its tx, so a test can pause a writer mid-rotation.
var testHookAfterKey = func() {}

// errStaleKey means a rotation landed between UpdateSealed selecting its key and the tx; the caller retries.
var errStaleKey = errors.New("vault: master key is stale")

// account is the keyring account for a vault's current master key; account+"."+kid is the
// immutable, never-deleted account every key — current or retired — is durably stored under,
// written BEFORE any ciphertext is committed under that kid (R1, R2, R4). account alone is a
// convenience pointer to whichever kid is current: resolveKey/matchKey fall back to the per-kid
// account whenever the pointer is stale, absent, or shared by a copied root mid-rotation.
func account(id string) string { return "vault-key/" + id }

// keyFlightKey names one resolution; concurrent callers with the same key share a single resolver run.
type keyFlightKey struct{ path, vaultID, kid, command string }

// keyFlight is one in-progress resolution; its result fields are written before done closes.
type keyFlight struct {
	done   chan struct{}
	key    *Key
	source string
	err    error
}

var keyCache struct {
	sync.Mutex
	keyFlightKey
	source  string
	key     *Key
	gen     uint64 // bumped by invalidateKey so a resolve that started earlier never refills the cache
	flights map[keyFlightKey]*keyFlight
}

// invalidateKey clears the per-process key cache so the next MasterKey re-resolves instead of serving a retired key.
func invalidateKey() {
	keyCache.Lock()
	defer keyCache.Unlock()
	keyCache.key = nil
	keyCache.gen++
	keyCache.flights = nil // later callers start a fresh resolve instead of joining a pre-invalidation one
}

func keyMeta() (id, kid, command string, err error) {
	err = View(func(t *Tx) error {
		id, kid, command = keyMetaTx(t)
		return nil
	})
	return
}

func keyMetaTx(t *Tx) (id, kid, command string) {
	id = vaultID(t)
	kid, _ = t.Get(Meta, "kid")
	command, _ = t.Get(Meta, "key_command")
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
		k, err := parseKey(v, id)
		if err != nil {
			return nil, "env", err
		}
		return matchKey(id, k, "env", kid, command)
	}
	fk := keyFlightKey{Path(), id, kid, command}
	keyCache.Lock()
	if keyCache.key != nil && keyCache.keyFlightKey == fk {
		k, source := keyCache.key, keyCache.source
		keyCache.Unlock()
		return k, source, nil
	}
	if f := keyCache.flights[fk]; f != nil {
		keyCache.Unlock()
		<-f.done // the resolver's keyring and key_command calls are each bounded by their own timeouts
		return f.key, f.source, f.err
	}
	f := &keyFlight{done: make(chan struct{})}
	if keyCache.flights == nil {
		keyCache.flights = map[keyFlightKey]*keyFlight{}
	}
	keyCache.flights[fk] = f
	gen := keyCache.gen
	keyCache.Unlock()
	runKeyFlight(f, fk, gen)
	return f.key, f.source, f.err
}

// runKeyFlight resolves fk outside the cache lock, then publishes the result to the cache and to f's waiters.
func runKeyFlight(f *keyFlight, fk keyFlightKey, gen uint64) {
	defer close(f.done)
	defer func() {
		keyCache.Lock()
		defer keyCache.Unlock()
		if keyCache.flights[fk] == f {
			delete(keyCache.flights, fk)
		}
		if f.err == nil && f.key != nil && keyCache.gen == gen {
			keyCache.keyFlightKey, keyCache.source, keyCache.key = fk, f.source, f.key
		}
	}()
	f.err = errors.New("vault: key resolution panicked") // overwritten below unless lookupKey panics
	f.key, f.source, f.err = lookupKey(fk.vaultID, fk.kid, fk.command)
}

// lookupKey is resolveKey's uncached path: the keyring account, then key_command, then matchKey's fallbacks.
func lookupKey(id, kid, command string) (*Key, string, error) {
	v, found, lookupErr := keyringLookup(account(id))
	recoverable := keyringRecoverable(lookupErr)
	if lookupErr != nil && !recoverable {
		return nil, "", lookupErr
	}
	var k *Key
	var err error
	source := "keyring"
	switch {
	case found:
		k, err = parsePointer(v, id, kid)
	case command != "":
		source = "command"
		k, err = runKeyCommand(command, id)
	case recoverable:
		return nil, "", wrapKeyringUnavailable(lookupErr)
	case kid == "":
		return nil, "", ErrLocked
	}
	if err != nil {
		return keyAfterError(id, source, kid, command, err)
	}
	return matchKey(id, k, source, kid, command)
}

// keyAfterError returns the per-kid key when a failed source left one recoverable, else err.
func keyAfterError(id, source, kid, command string, err error) (*Key, string, error) {
	if kid != "" {
		if fk, fsource, _ := fallbackKey(id, source, kid, command); fk != nil {
			return fk, fsource, nil
		}
	}
	return nil, source, err
}

// parsePointer parses the keyring's current-key pointer; with a kid recorded a malformed pointer reads as absent.
func parsePointer(v, id, kid string) (*Key, error) {
	k, err := parseKey(v, id)
	if err != nil && kid != "" {
		// Malformed pointer with a kid recorded: the pointer is only a convenience cache
		// of the immutable per-kid account, so fall to matchKey's lookup below instead.
		return nil, nil
	}
	return k, err
}

// keyringRecoverable reports a keyring that could not positively establish absence (no tool, locked, timed out).
func keyringRecoverable(err error) bool {
	return errors.Is(err, ErrKeyringUnavailable) || errors.Is(err, ErrKeyringNoAnswer)
}

// parseKeyCommandArgv decodes meta/key_command as a JSON argv array, so no shell or quoting grammar is involved (R6).
func parseKeyCommandArgv(raw string) ([]string, error) {
	var argv []string
	if err := json.Unmarshal([]byte(raw), &argv); err != nil {
		return nil, fmt.Errorf("vault: key_command must be a JSON array of strings: %w", err)
	}
	if len(argv) == 0 {
		return nil, errors.New("vault: key_command must not be an empty argv array")
	}
	if argv[0] == "" {
		return nil, errors.New("vault: key_command argv[0] (the executable) must not be empty")
	}
	return argv, nil
}

// runKeyCommand runs raw's decoded argv directly (no shell) and parses its stdout as a master key for vaultID.
func runKeyCommand(raw, vaultID string) (*Key, error) {
	argv, err := parseKeyCommandArgv(raw)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = waitDelay
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("vault: key_command: %w", ctx.Err())
	}
	if err != nil { // never quote the command's stderr: a key tool may print key material there
		return nil, fmt.Errorf("vault: key_command: %w (output not shown; run it yourself to see why)", err)
	}
	return parseKey(string(out), vaultID)
}

// matchKey resolves k against the vault's recorded kid: a direct hit returns k unchanged; a
// mismatch falls back to the immutable per-kid account, then to key_command if one is configured
// and wasn't already the source, before giving up with an error naming every source tried (R1,
// R2). A genuine keyring error on the fallback lookup is folded into that error text, never
// surfaced raw, so a locked/no-tool keyring never masquerades as "wrong key".
func matchKey(id string, k *Key, source, kid, command string) (*Key, string, error) {
	if k != nil && (kid == "" || k.Kid == kid) {
		return k, source, nil
	}
	if kid == "" {
		return nil, "", ErrLocked
	}
	fk, fsource, tried := fallbackKey(id, source, kid, command)
	switch {
	case fk != nil:
		return fk, fsource, nil
	case k == nil:
		return nil, "", ErrLocked
	}
	return nil, source, fmt.Errorf("%w: tried %s", ErrWrongKey, strings.Join(tried, ", "))
}

// fallbackKey tries the per-kid account, then key_command unless it was the source; a miss returns every source tried.
func fallbackKey(id, source, kid, command string) (*Key, string, []string) {
	tried := []string{source}
	rk, label := perKidKey(id, kid)
	if rk != nil {
		return rk, "keyring", nil
	}
	tried = append(tried, label)
	if command == "" || source == "command" {
		return nil, "", tried
	}
	ck, label := commandKey(command, id, kid)
	if ck != nil {
		return ck, "command", nil
	}
	return nil, "", append(tried, label)
}

// perKidKey reads kid's immutable keyring account; a miss returns the label matchKey's error names it by.
func perKidKey(id, kid string) (*Key, string) {
	v, found, err := keyringLookup(account(id) + "." + kid)
	switch {
	case err == nil && found:
		if rk, perr := parseKey(v, id); perr == nil && rk.Kid == kid {
			return rk, ""
		}
	case err != nil:
		return nil, fmt.Sprintf("keyring (%v)", err)
	}
	return nil, "keyring"
}

// commandKey runs key_command for kid; a miss returns the label matchKey's error names it by.
func commandKey(command, id, kid string) (*Key, string) {
	ck, cerr := runKeyCommand(command, id)
	switch {
	case cerr != nil:
		return nil, fmt.Sprintf("key_command (%v)", cerr)
	case ck.Kid == kid:
		return ck, ""
	}
	return nil, "key_command"
}

// wrapKeyringUnavailable turns a keyring that cannot establish absence into a locked vault, not a fresh key (R6).
func wrapKeyringUnavailable(err error) error {
	if keyringRecoverable(err) {
		return fmt.Errorf("%w: set HARNESS_VAULT_KEY or meta/key_command (%v)", ErrLocked, err)
	}
	return err
}

func EnsureKey() (*Key, error) {
	// Materialize the vault file (and its real vault_id) before any key resolution: the HKDF
	// subkey is salted by vault_id, so deriving one against a not-yet-created vault's empty id
	// would mismatch the id the first write actually persists.
	if _, err := ensureVaultID(); err != nil {
		return nil, err
	}
	k, _, err := MasterKey()
	if err == nil {
		return k, nil
	}
	if !errors.Is(err, ErrLocked) {
		return nil, wrapKeyringUnavailable(err)
	}
	var result *Key
	err = paths.WithFileLock(Path()+".key", func() error {
		var e error
		result, e = ensureKeyLocked()
		return e
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ensureKeyLocked re-resolves under the "<vault>.key" lock and generates a genesis key only on a proven absence.
func ensureKeyLocked() (*Key, error) {
	k, _, err := MasterKey()
	if err == nil {
		return k, nil
	}
	if !errors.Is(err, ErrLocked) {
		return nil, wrapKeyringUnavailable(err)
	}
	id, err := ensureVaultID()
	if err != nil {
		return nil, err
	}
	_, kid, _, err := keyMeta()
	if err != nil {
		return nil, err
	}
	if kid != "" {
		// A kid is recorded but no source above could open it: locked, not absent.
		return nil, ErrLocked
	}
	_, found, lookupErr := keyringLookup(account(id))
	if lookupErr != nil {
		return nil, wrapKeyringUnavailable(lookupErr)
	}
	if found {
		// A key is already in the keyring; meta/kid being empty here would be a corrupt
		// state, not an invitation to generate a second one.
		return nil, ErrLocked
	}
	return genesisKey(id)
}

// genesisKey generates vault id's first master key and records it in the keyring and meta/kid.
func genesisKey(id string) (*Key, error) {
	newK, value, err := randomKey(id)
	if err != nil {
		return nil, err
	}
	// Persist the genesis key under its immutable per-kid account before the convenience
	// pointer, so matchKey can always find it even if a copied root's pointer diverges later.
	if err := keyringSet(account(id)+"."+newK.Kid, value); err != nil {
		return nil, err
	}
	if err := keyringSet(account(id), value); err != nil {
		return nil, err
	}
	if err := storeKeyID(newK); err != nil {
		return nil, err
	}
	return newK, nil
}

// ensureVaultID creates the vault file and its random vault_id when absent, and returns the id.
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

func randomKey(vaultID string) (*Key, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, "", err
	}
	value := base64.StdEncoding.EncodeToString(raw)
	k, err := parseKey(value, vaultID)
	return k, value, err
}

// resealSecrets re-seals every secret from old to next inside t, retires old's kid, and advances meta/kid.
func resealSecrets(t *Tx, old, next *Key) error {
	curKid, _ := t.Get(Meta, "kid")
	if curKid != "" && curKid != old.Kid {
		return ErrWrongKey
	}
	for _, bucket := range []string{Secrets, SettingsBackups} {
		if err := resealBucket(t, bucket, old, next); err != nil {
			return err
		}
	}
	if err := retireKid(t, old.Kid); err != nil {
		return err
	}
	return t.Put(Meta, "kid", next.Kid)
}

func resealBucket(t *Tx, bucket string, old, next *Key) error {
	for _, name := range t.Names(bucket) {
		r, ok, rerr := t.RecordIn(bucket, name)
		if rerr != nil {
			return rerr
		}
		if !ok {
			return fmt.Errorf("vault: invalid record %q", name)
		}
		plain, e := old.openIn(bucket, name, r)
		if e != nil {
			return e
		}
		if bucket == SettingsBackups {
			r.Sealed = next.SealSettingsBackup(name, plain)
		} else {
			r.Sealed = next.Seal(name, plain)
		}
		r.Kid = next.Kid
		if e := t.PutRecordIn(bucket, name, r); e != nil {
			return e
		}
	}
	return nil
}

// rotateKeyLocked runs the body of RotateKey under the "<vault>.key" file lock.
func rotateKeyLocked(force bool) (oldKid, newKid string, err error) {
	defer invalidateKey()
	old, id, e := rotatePreflight(force)
	if old != nil {
		oldKid = old.Kid
	}
	if e != nil {
		return oldKid, "", e
	}
	acct := account(id)

	// Retain the outgoing key under its immutable kid account; idempotent if an earlier
	// rotation (or this one, resumed after a crash) already wrote it.
	if e := keyringSet(acct+"."+old.Kid, old.raw); e != nil {
		return oldKid, "", e
	}

	next, value, e := randomKey(id)
	if e != nil {
		return oldKid, "", e
	}
	newKid = next.Kid

	// Persist the new key under its immutable kid account BEFORE any ciphertext is committed under it (R1).
	if e := keyringSet(acct+"."+newKid, value); e != nil {
		return oldKid, newKid, e
	}

	if e := Update(func(t *Tx) error { return resealSecrets(t, old, next) }); e != nil {
		return oldKid, newKid, e
	}
	// Convenience pointer only, updated after the atomic reseal commits; resolveKey/matchKey
	// never depend on this being current (R1, R2).
	if e := keyringSet(acct, value); e != nil {
		return oldKid, newKid, e
	}
	audit.Log(audit.KeyRotate, oldKid+"->"+newKid, "")
	return oldKid, newKid, nil
}

// rotatePreflight resolves the outgoing keyring-held key and checks meta/kid and the guard; old is nil if unresolved.
func rotatePreflight(force bool) (old *Key, id string, err error) {
	old, source, err := MasterKey()
	if err != nil {
		return nil, "", err
	}
	if source != "keyring" && (source != "env" || !force) { // --force rotates an env-held key; the caller updates the env
		return nil, "", errors.New("vault: rotation requires a keyring-held master key")
	}
	id, kid, command, err := keyMeta()
	if err != nil {
		return old, "", err
	}
	if kid != "" && kid != old.Kid {
		return old, "", ErrWrongKey
	}
	if err := rotateGuard(force, command); err != nil {
		return old, "", err
	}
	return old, id, nil
}

// RotateKey re-seals every secret under a new master key, retaining every key under its own kid account (R1, R2, R4).
//
// The new key is stored before the re-seal tx and the pointer moves after it commits, so a crash strands nothing.
// force bypasses the guard against rotating while HARNESS_VAULT_KEY or meta/key_command is set: update that source
// with the new key immediately after.
func RotateKey(force bool) (oldKid, newKid string, err error) {
	err = paths.WithFileLock(Path()+".key", func() error {
		var e error
		oldKid, newKid, e = rotateKeyLocked(force)
		return e
	})
	return
}

// rotateGuard refuses rotation while HARNESS_VAULT_KEY or key_command supplies the key, unless forced.
func rotateGuard(force bool, command string) error {
	switch {
	case force:
		return nil
	case os.Getenv("HARNESS_VAULT_KEY") != "":
		return errors.New("vault: HARNESS_VAULT_KEY is set; rotating would silently break it — " +
			"update the env var with the new key right after, or pass --force")
	case command != "":
		return errors.New("vault: meta/key_command is set; rotating would silently break it — " +
			"update that command's output with the new key right after, or pass --force")
	}
	return nil
}

// retireKid appends kid to meta/retired, reading it raw: a corrupt list refuses rather than losing history.
func retireKid(t *Tx, kid string) error {
	retired, err := Retired(t)
	if err != nil {
		return fmt.Errorf("refusing to rotate (history would be lost): %w", err)
	}
	if !slices.Contains(retired, kid) {
		retired = append(retired, kid)
	}
	data, err := json.Marshal(retired)
	if err != nil {
		return err
	}
	return t.Put(Meta, "retired", string(data))
}

// UpdateSealed picks a key outside the tx and checks meta/kid inside it; a stale key is retried, up to 3 attempts.
func UpdateSealed(fn func(*Tx, *Key) error) error {
	var err error
	for range 3 {
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

// ReadSecret reads a record and its kid in one View, opens it with that kid's key, retrying once on a rotation (R3).
func ReadSecret(name string) (value string, found bool, err error) {
	return readSealed(Secrets, name)
}

// ReadSettingsBackup opens a config record using the same coherent key lookup as credentials.
func ReadSettingsBackup(name string) (value string, found bool, err error) {
	return readSealed(SettingsBackups, name)
}

// ReadSecrets opens several records in one View and one key lookup; absent names are left out of the map.
func ReadSecrets(names []string) (map[string]string, error) {
	return readSealedAll(Secrets, names)
}

func readSealed(bucket, name string) (value string, found bool, err error) {
	vals, err := readSealedAll(bucket, []string{name})
	if err != nil {
		return "", false, err
	}
	value, found = vals[name]
	return value, found, nil
}

// readSealedAll is ReadSecret for many names: one View, that kid's key, one retry on a concurrent rotation (R3).
func readSealedAll(bucket string, names []string) (map[string]string, error) {
	for attempt := range 2 {
		var id, kid, command string
		records := map[string]Record{}
		if verr := View(func(t *Tx) error {
			id, kid, command = keyMetaTx(t)
			for _, name := range names {
				r, ok, rerr := t.RecordIn(bucket, name)
				if rerr != nil {
					return rerr
				}
				if ok {
					records[name] = r
				}
			}
			return nil
		}); verr != nil {
			return nil, verr
		}
		vals, err := openAll(bucket, records, id, kid, command)
		if errors.Is(err, ErrWrongKey) && attempt == 0 {
			invalidateKey()
			continue
		}
		return vals, err
	}
	return nil, ErrWrongKey
}

// openAll decrypts records with the key for kid; no records means no key lookup.
func openAll(bucket string, records map[string]Record, id, kid, command string) (map[string]string, error) {
	vals := make(map[string]string, len(records))
	if len(records) == 0 {
		return vals, nil
	}
	k, _, err := resolveKey(id, kid, command)
	if err != nil {
		return nil, err
	}
	for name, r := range records {
		v, err := k.openIn(bucket, name, r)
		if err != nil {
			return nil, err
		}
		vals[name] = v
	}
	return vals, nil
}

// Each encrypted collection derives a distinct subkey from the same vault-scoped root.
const secretsInfo = "claude-code/vault/v1/secrets"
const settingsBackupsInfo = "claude-code/vault/v1/settings-backups"

type Key struct {
	Kid         string
	aead        cipher.AEAD
	backupsAEAD cipher.AEAD
	raw         string // the base64 value this key was parsed from; rotation retains it as-is (R4)
}

// parseKey turns a base64 master-key value into a Key scoped to vaultID. Kid identifies the root
// itself (stable across vaults so retired-key accounts and matchKey stay vault-independent); the
// AEAD seals under K = HKDF(sha256, root, salt=vaultID, info=secretsInfo, 32) so a root reused
// across two vaults still yields distinct ciphertext keys, without changing who can decrypt.
func parseKey(value, vaultID string) (*Key, error) {
	trimmed := strings.TrimSpace(value)
	root, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil || len(root) != 32 {
		return nil, errors.New("vault: master key must be base64-encoded 32 bytes")
	}
	aead, err := newAEAD(root, vaultID, secretsInfo)
	if err != nil {
		return nil, err
	}
	backupAEAD, err := newAEAD(root, vaultID, settingsBackupsInfo)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(root)
	return &Key{Kid: hex.EncodeToString(sum[:])[:16], aead: aead, backupsAEAD: backupAEAD, raw: trimmed}, nil
}

// newAEAD derives the subkey for info from root and vaultID and wraps it in AES-GCM with random nonces.
func newAEAD(root []byte, vaultID, info string) (cipher.AEAD, error) {
	sub, err := hkdf.Key(sha256.New, root, []byte(vaultID), info, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(sub)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCMWithRandomNonce(block)
}

// Raw returns the base64-encoded master key value k was parsed from — the same encoding
// HARNESS_VAULT_KEY accepts. Callers must only print it through an explicit, audited reveal path.
func (k *Key) Raw() string { return k.raw }

func (k *Key) Seal(name, value string) []byte {
	return k.aead.Seal(nil, nil, []byte(value), []byte(secretsInfo+"/"+name))
}

func (k *Key) Open(name string, r Record) (string, error) {
	return k.open(k.aead, secretsInfo, name, r)
}

// open checks r was sealed under k, then decrypts it with aead and the info-scoped AAD.
func (k *Key) open(aead cipher.AEAD, info, name string, r Record) (string, error) {
	if r.Kid != k.Kid {
		return "", ErrWrongKey
	}
	v, err := aead.Open(nil, nil, r.Sealed, []byte(info+"/"+name))
	return string(v), err
}

// SealSettingsBackup encrypts a snapshot without sharing the credential collection's key or AAD.
func (k *Key) SealSettingsBackup(name, value string) []byte {
	return k.backupsAEAD.Seal(nil, nil, []byte(value), []byte(settingsBackupsInfo+"/"+name))
}

func (k *Key) openIn(bucket, name string, r Record) (string, error) {
	if bucket == Secrets {
		return k.Open(name, r)
	}
	return k.open(k.backupsAEAD, settingsBackupsInfo, name, r)
}

// MasterKeyStatus returns the secret count and master-key source; err means the vault or its key will not open.
func MasterKeyStatus() (count int, source string, err error) {
	names, err := NamesErr()
	if err != nil {
		return 0, "", err
	}
	count = len(names)
	if count == 0 {
		return 0, "not needed", nil
	}
	_, source, err = MasterKey()
	if err != nil {
		return count, source, err
	}
	_, err = ReadSecrets(names) // opening every record proves the key decrypts them, not merely resolves
	return count, source, err
}
