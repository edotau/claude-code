package agent

var (
	// VendorTargetNote exposes vendorTargetNote to the external parity test (which imports launch).
	VendorTargetNote = vendorTargetNote

	BackupAndWrite      = backupAndWrite
	CodexHookSourcePath = codexHookSourcePath
	CodexTrustEntries   = codexTrustEntries
	InsertTopLevelTOML  = insertTopLevelTOML
	LostLines           = lostLines
	MergeCodexHooks     = mergeCodexHooks
	MergeCodexTrust     = mergeCodexTrust
	MergeCopilotHooks   = mergeCopilotHooks
	MirrorsOf           = mirrorsOf
)
