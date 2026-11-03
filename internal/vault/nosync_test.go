package vault

// vault's own tests cannot import vaulttest (cycle).
func init() { DisableSyncForTests() }
