#!/usr/bin/env python3
"""
assumption_linter.py — Detect hidden assumptions in a plan or proposal.

Principle: Think Before Coding — "State assumptions explicitly. If uncertain,
ask. If multiple interpretations exist, present them — don't pick silently."

Reads a markdown plan (or stdin) and flags:
  - Minimizing language that hides complexity ("just", "simply", "obviously")
  - Hopeful-not-verified phrasing ("should work", "probably")
  - Absolute scope claims ("all users", "always", "never")
  - Vague action verbs without a measurable target ("fix", "improve", "optimize")
  - Unscoped subjects ("the user" — which user?)
  - Numbered plan blocks with no verification step

Heuristic, not a proof engine — false positives are expected. The point is to
trigger a conversation about assumptions before code is written.

Usage:
    assumption_linter.py plan.md
    echo "I'll just export the user data" | assumption_linter.py -
    assumption_linter.py plan.md --json
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path

# (pattern, category, message) — phrase-level signals checked on every line.
SIGNALS = [
    (
        re.compile(r"\b(?:just|simply|simple|straightforward|trivial|easy)\b", re.I),
        "minimizing",
        "Minimizing language often hides complexity. What's being skipped?",
    ),
    (
        re.compile(r"\b(?:obviously|clearly|of course|naturally)\b", re.I),
        "unstated-assumption",
        "Signals an unstated assumption. Is it really obvious?",
    ),
    (
        re.compile(r"\b(?:should\s+(?:be\s+fine|work)|shouldn't\s+be\s+a\s+problem|probably)\b", re.I),
        "hopeful",
        "Hopeful, not verified. How will you confirm?",
    ),
    (
        re.compile(r"\b(?:I\s+assume|assuming|I'm\s+guessing)\b", re.I),
        "explicit-assumption",
        "Explicit — good — but have you verified it?",
    ),
    (
        re.compile(r"\b(?:all\s+users|every|everything|always|never)\b", re.I),
        "absolute-scope",
        "Absolute scope. Is that really the case for every input?",
    ),
    (
        re.compile(r"\b(?:fix|improve|optimize|refactor|update|enhance)\b(?!\s+\w+\s+(?:to|so|by))", re.I),
        "vague-action",
        "Vague action verb. What specifically changes? What's the measurable result?",
    ),
    (
        re.compile(r"\b(?:the\s+user|users)\b(?!.*\b(?:who|which|admin|role|authenticated|specific)\b)", re.I),
        "unscoped-subject",
        "Which user(s)? All? A specific role? Authenticated only?",
    ),
]

STEP_BLOCK = re.compile(r"(?:^|\n)((?:[ \t]*\d+[.)]\s+.+\n?)+)")
VERIFY_WORD = re.compile(r"\b(?:test|verify|check|assert|confirm|ensure|validate)\b", re.I)


def lint_text(text: str) -> list[dict]:
    findings: list[dict] = []
    for i, line in enumerate(text.splitlines(), 1):
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        for pattern, category, message in SIGNALS:
            match = pattern.search(stripped)
            if match:
                findings.append(
                    {
                        "line": i,
                        "category": category,
                        "matched": match.group(0),
                        "message": message,
                        "context": stripped[:120],
                    }
                )

    for block in STEP_BLOCK.findall(text):
        if not VERIFY_WORD.search(block):
            findings.append(
                {
                    "line": 0,
                    "category": "missing-verification",
                    "matched": block.strip()[:80].replace("\n", " "),
                    "message": "Plan block has no verification step. Add a 'verify:' check to each step.",
                    "context": block.strip()[:120].replace("\n", " "),
                }
            )
    return findings


def main() -> None:
    ap = argparse.ArgumentParser(
        description="Detect hidden assumptions in a plan or proposal (Think Before Coding).",
        epilog="Reads a markdown file or stdin. Flags silent choices, vague actions, missing verification.",
    )
    ap.add_argument("input", nargs="?", default="-", help="Markdown file, or - for stdin")
    ap.add_argument("--json", action="store_true")
    args = ap.parse_args()

    if args.input == "-":
        text, source = sys.stdin.read(), "stdin"
    else:
        path = Path(args.input)
        if not path.exists():
            print(f"[error] {path} not found", file=sys.stderr)
            sys.exit(0)
        text, source = path.read_text(encoding="utf-8", errors="replace"), str(path)

    findings = lint_text(text)
    by_cat: dict[str, list[dict]] = {}
    for f in findings:
        by_cat.setdefault(f["category"], []).append(f)

    verdict = "CLEAN" if not findings else ("REVIEW" if len(findings) < 5 else "CLARIFY")
    result = {
        "status": "ok",
        "source": source,
        "total_findings": len(findings),
        "by_category": {k: len(v) for k, v in by_cat.items()},
        "verdict": verdict,
        "findings": findings,
    }

    if args.json:
        print(json.dumps(result, indent=2))
        return

    print(f"Assumption Linter — {source}")
    print(f"Findings: {len(findings)}   Verdict: {verdict}")
    if not findings:
        print("\n  Plan looks explicit. Assumptions are surfaced.")
        return
    print()
    for cat, items in by_cat.items():
        print(f"  [{cat}] ({len(items)})")
        for item in items[:5]:
            ref = f"L{item['line']}: " if item["line"] else ""
            print(f"    {ref}{item['message']}")
            print(f'      -> "{item["matched"]}" in: {item["context"][:80]}')
        if len(items) > 5:
            print(f"    ... and {len(items) - 5} more")
        print()


if __name__ == "__main__":
    main()
