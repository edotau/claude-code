#!/usr/bin/env python3
"""
diff_surgeon.py — Detect diff noise: changes that don't trace to the stated goal.

Principle: Surgical Changes — "Every changed line should trace directly to the
user's request." Drive-by reformatting, comment churn, and style swaps inflate a
diff and hide the real change from reviewers.

Flags, per file in a unified diff:
  - Whitespace-only changes
  - Comment-only changes
  - Docstring additions
  - Quote-style swaps (only the quote char differs between a -/+ pair)

Usage:
    diff_surgeon.py                       # staged diff (git diff --cached)
    diff_surgeon.py --diff HEAD~1..HEAD   # a commit range
    diff_surgeon.py --file changes.diff   # a saved diff
    diff_surgeon.py --json

Exit code is always 0 — advisory signal, not a gate. The verdict
(CLEAN/NOISY/VERY_NOISY) and noise ratio carry the result.
"""

from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys
from pathlib import Path

WHITESPACE_ONLY = re.compile(r"^[+-]\s*$")
COMMENT_ONLY = re.compile(r"^[+-]\s*(?:#|//|/\*|\*/|\*|<!--)")
DOCSTRING_ADD = re.compile(r'^[+]\s*(?:"""|\'\'\')')


def get_diff(args: argparse.Namespace) -> str:
    if args.file:
        return Path(args.file).read_text(encoding="utf-8", errors="replace")
    cmd = ["git", "diff", "--cached"] if not args.diff else ["git", "diff", args.diff]
    try:
        return subprocess.run(cmd, capture_output=True, text=True, timeout=30, check=False).stdout
    except (subprocess.TimeoutExpired, FileNotFoundError) as exc:
        print(f"[error] git diff failed: {exc}", file=sys.stderr)
        sys.exit(0)


def parse_files(diff_text: str) -> list[dict]:
    """Split a unified diff into per-file lists of changed (+/-) body lines.

    Hunk headers (@@) and file headers (+++/---) are skipped so they never count
    as changes.
    """
    files: list[dict] = []
    current: dict | None = None
    for line in diff_text.splitlines():
        if line.startswith("diff --git"):
            current = {"file": line.split(" b/")[-1], "changes": []}
            files.append(current)
        elif current is None or line.startswith(("+++", "---", "@@")):
            continue
        elif line and line[0] in "+-":
            current["changes"].append(line)
    return files


def classify(line: str) -> str | None:
    """Return a noise category for a changed line, or None if it looks intentional."""
    if WHITESPACE_ONLY.match(line):
        return "whitespace"
    if COMMENT_ONLY.match(line):
        return "comment-only"
    if DOCSTRING_ADD.match(line):
        return "docstring-addition"
    return None


def detect_quote_swaps(changes: list[str]) -> list[dict]:
    """Find -/+ pairs identical except for quote characters."""
    adds = sorted(c for c in changes if c.startswith("+"))
    dels = sorted(c for c in changes if c.startswith("-"))
    findings = []
    for a, d in zip(adds, dels):
        a_body, d_body = a[1:].strip(), d[1:].strip()
        if a_body != d_body and a_body.replace('"', "'") == d_body.replace('"', "'"):
            findings.append({"category": "quote-style-swap", "line": f"{d[:60]} -> {a[:60]}"})
    return findings


def analyze_file(file_data: dict) -> list[dict]:
    findings = [{"category": cat, "line": line[:120]} for line in file_data["changes"] if (cat := classify(line))]
    findings += detect_quote_swaps(file_data["changes"])
    return findings


def build_result(files: list[dict]) -> dict:
    file_results = []
    total_noise = 0
    total_changes = 0
    for fd in files:
        total_changes += len(fd["changes"])
        findings = analyze_file(fd)
        if findings:
            total_noise += len(findings)
            file_results.append({"file": fd["file"], "findings": findings})
    ratio = round(total_noise / total_changes, 2) if total_changes else 0.0
    verdict = "CLEAN" if ratio < 0.1 else ("NOISY" if ratio < 0.3 else "VERY_NOISY")
    return {
        "status": "ok",
        "files_in_diff": len(files),
        "total_change_lines": total_changes,
        "noise_lines": total_noise,
        "noise_ratio": ratio,
        "verdict": verdict,
        "file_results": file_results,
    }


def print_report(result: dict) -> None:
    print(f"Diff Surgeon — {result['files_in_diff']} files, {result['total_change_lines']} changed lines")
    print(f"Noise ratio: {result['noise_ratio']:.0%} ({result['noise_lines']} noise lines)")
    print(f"Verdict: {result['verdict']}")
    if not result["file_results"]:
        print("\n  All changes look intentional. Clean diff.")
        return
    print()
    for fr in result["file_results"]:
        print(f"  {fr['file']}:")
        by_cat: dict[str, list[str]] = {}
        for f in fr["findings"]:
            by_cat.setdefault(f["category"], []).append(f["line"])
        for cat, lines in by_cat.items():
            print(f"    [{cat}] {len(lines)} instance(s)")
            for line in lines[:3]:
                print(f"      {line}")
            if len(lines) > 3:
                print(f"      ... and {len(lines) - 3} more")
    print("\nRecommendation: review flagged lines. Remove changes that don't trace to your task.")


def main() -> None:
    ap = argparse.ArgumentParser(
        description="Detect diff noise — changes that don't trace to the stated goal (Surgical Changes).",
        epilog="Run before committing to catch drive-by refactors and style drift.",
    )
    ap.add_argument("--diff", default=None, help="Git diff range (e.g. HEAD~1..HEAD). Default: staged.")
    ap.add_argument("--file", default=None, help="Read diff from a file instead of git")
    ap.add_argument("--json", action="store_true")
    args = ap.parse_args()

    diff_text = get_diff(args)
    if not diff_text.strip():
        result = {"status": "ok", "files": 0, "noise_lines": 0, "verdict": "CLEAN", "message": "No diff to analyze"}
        print(
            json.dumps(result, indent=2) if args.json else "No diff to analyze. Stage changes (git add) or pass --diff."
        )
        return

    result = build_result(parse_files(diff_text))
    if args.json:
        print(json.dumps(result, indent=2))
        return
    print_report(result)


if __name__ == "__main__":
    main()
