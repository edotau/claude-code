package vault

// MasterKeyStatus resolves the vault's secret count and master-key source in one place, shared by the
// "vault master key" doctor row and launch's "vault" summary row so both describe the same check.
// count is 0 and err is non-nil when NamesErr itself failed (vault unreadable); count is 0 and err is
// nil when the vault holds no secrets (no master key needed); count > 0 and err non-nil when the
// resolved master key cannot open the vault.
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
