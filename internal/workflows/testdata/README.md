# Workflow golden fixtures

## skeleton/ (claude-code workflow skeleton)
Pattern output was frozen from `workflow_skeleton.py` before its retirement; that script is no longer a live oracle.
No file input — CLI args only. No cwd sensitivity (no path embedded in output):

    claude-code workflow skeleton pipeline --name review-changes
    claude-code workflow skeleton parallel --name fan-out-audit
    claude-code workflow skeleton evaluator --name gen-verify
    claude-code workflow skeleton orchestrator --name plan-then-build
    claude-code workflow skeleton pipeline              (default-name.want)

## Embedded asset migration (2026-11-24)

`prompts/outputs.json` freezes SHA-256 digests of 1,456 outputs captured from the Go module's original five workflow
scripts before wave 6 extraction. Its 52 accessors cover 71 source units, with tables grouped, across 28 fixed input
cases: file/range/split modes, empty/populated inputs, rules overrides, finding kinds, and all evaluator scenarios and
method/control conditions. The remaining 21 shared-block units moved byte for byte; assets tests pin their hashes.

`prompts/check.cjs` supplies these fixed inputs to accessors in an isolated JavaScript context, compares rendered
output digests, and requires the manifest to name every accessor. Keep the checker and manifest together when adding
accessors. These frozen migration digests are historical evidence; do not regenerate them from migrated code.

`skeleton/migration.json` freezes 16 complete CLI output digests from the pre-migration binary: all four patterns with
default, quote, backslash/newline/carriage-return, and Unicode names. The original `.want` files remain unchanged.
