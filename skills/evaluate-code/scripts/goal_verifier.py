#!/usr/bin/env python3
"""
goal_verifier.py — Check whether a plan has verifiable success criteria.

Principle: Goal-Driven Execution — "Define success criteria. Loop until
verified. Don't tell it what to do — give it success criteria and watch it go."

Reads a markdown plan and scores each step 0-3 on verification quality:
    3  concrete    — a runnable check (assert, pytest/jest, exit 0, status 200, metric)
    2  reasonable  — a manual/visual check (verify, confirm, inspect, review)
    1  vague       — "should work", "looks right"
    0  none        — no verification mentioned

Usage:
    goal_verifier.py plan.md
    goal_verifier.py plan.md --json
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path

CONCRETE = re.compile(
    r"\b(?:assert\w*|expect\(|\.toBe|\.toEqual|pytest|jest|npm\s+test|go\s+test|"
    r"exit\s*(?:code)?\s*[=:]?\s*0|status\s*[=:]?\s*200|curl\b|grep\b|diff\b|"
    r"benchmark|latency\s*<|throughput\s*>)\b",
    re.I,
)
REASONABLE = re.compile(
    r"\b(?:verify|check|confirm|inspect|review|compare|validate|"
    r"run\s+and\s+see|manually|open\s+in\s+browser|screenshot)\b",
    re.I,
)
VAGUE = re.compile(
    r"\b(?:should\s+work|looks?\s+(?:good|right|fine|ok)|seems?\s+(?:correct|fine)|" r"hopefully|probably\s+works?)\b",
    re.I,
)
STEP = re.compile(r"^(?:\d+[.)]\s+|[-*]\s+\[.\]\s+|[-*]\s+Step\s+\d+)", re.M)
FINAL = re.compile(r"\b(?:final|end-to-end|full\s+test|regression|all\s+(?:tests?\s+)?pass)\b", re.I)


def extract_steps(text: str) -> list[str]:
    steps: list[str] = []
    current: list[str] | None = None
    for line in text.splitlines():
        if STEP.match(line.strip()):
            if current is not None:
                steps.append("\n".join(current))
            current = [line.strip()]
        elif current is not None:
            current.append(line)
    if current is not None:
        steps.append("\n".join(current))
    return steps


def score_step(step: str) -> tuple[int, str]:
    if CONCRETE.search(step):
        return 3, "concrete"
    if REASONABLE.search(step):
        return 2, "reasonable"
    if VAGUE.search(step):
        return 1, "vague"
    return 0, "none"


def analyze(text: str, source: str) -> dict:
    steps = extract_steps(text)
    if not steps:
        return {
            "status": "ok",
            "source": source,
            "steps_found": 0,
            "verdict": "NO_PLAN",
            "score": 0,
            "max_score": 0,
            "step_results": [],
            "recommendations": ["No numbered/bulleted steps found. Is this a plan?"],
        }

    results = []
    total = 0
    for step in steps:
        pts, level = score_step(step)
        total += pts
        results.append({"title": step.splitlines()[0][:120], "score": pts, "level": level})

    max_score = len(steps) * 3
    pct = round(total / max_score * 100, 1) if max_score else 0.0
    has_final = bool(FINAL.search(steps[-1]))
    verdict = "STRONG" if pct >= 70 else ("WEAK" if pct >= 40 else "MISSING")

    recs = []
    none_n = sum(1 for r in results if r["level"] == "none")
    vague_n = sum(1 for r in results if r["level"] == "vague")
    if none_n:
        recs.append(f"{none_n} step(s) have no verification. Add 'verify: [check]' to each.")
    if vague_n:
        recs.append(f"{vague_n} step(s) have vague criteria. Replace 'should work' with a concrete check.")
    if not has_final:
        recs.append("No final / end-to-end verification step. Add one at the end.")
    if not recs:
        recs.append("Strong verification coverage. Good to go.")

    return {
        "status": "ok",
        "source": source,
        "steps_found": len(steps),
        "score": total,
        "max_score": max_score,
        "percentage": pct,
        "has_final_verification": has_final,
        "verdict": verdict,
        "step_results": results,
        "recommendations": recs,
    }


def main() -> None:
    ap = argparse.ArgumentParser(
        description="Check whether a plan has verifiable success criteria (Goal-Driven Execution).",
        epilog="Scores each step 0-3 by verification quality.",
    )
    ap.add_argument("input", nargs="?", default="-", help="Markdown plan file, or - for stdin")
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

    result = analyze(text, source)
    if args.json:
        print(json.dumps(result, indent=2))
        return

    print(f"Goal Verifier — {source}")
    print(
        f"Steps: {result['steps_found']}   Score: {result['score']}/{result['max_score']} ({result.get('percentage', 0)}%)"
    )
    print(f"Verdict: {result['verdict']}\n")
    icons = {"concrete": "+", "reasonable": "~", "vague": "?", "none": "!"}
    for sr in result["step_results"]:
        print(f"  [{icons[sr['level']]}] {sr['title'][:100]}  ({sr['level']}, {sr['score']}/3)")
    print()
    for rec in result["recommendations"]:
        print(f"  -> {rec}")


if __name__ == "__main__":
    main()
