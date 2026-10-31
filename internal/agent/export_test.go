package agent

import "github.com/edotau/claude-code/internal/providers"

var (
	// VendorTargetNote exposes Schema.targetNote to the external parity test (which imports launch).
	VendorTargetNote = func(req RunOptions, name string) (providers.Target, bool, string, error) {
		return schemaFor(name).targetNote(req)
	}

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
