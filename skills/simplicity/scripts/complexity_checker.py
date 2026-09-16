#!/usr/bin/env python3
"""
complexity_checker.py — Detect over-engineering in Python / TypeScript files.

Principle: Simplicity First — "No abstractions for single-use code. If you
write 200 lines and it could be 50, rewrite it."

Checks per file:
  - Cyclomatic complexity (PER FUNCTION, not a file average)
  - Function length (long functions do too much)
  - Nesting depth (deep nesting is hard to read)
  - Class density (too many classes for the file size = premature abstraction)
  - Import count (many imports = over-coupling)
  - Premature ABC / Protocol in a small file

Python files are analyzed with the `ast` module (accurate per-function metrics).
TypeScript/JavaScript fall back to line/regex heuristics — adequate for a warn-only
signal, never used for Python where precision matters.

Usage:
    complexity_checker.py path/to/file.py
    complexity_checker.py a.py b.py src/ --threshold strict
    complexity_checker.py . --ext py,ts --json

Thresholds: strict (new code) | medium (default) | relaxed (legacy).
Exit code is always 0 — this is an advisory signal, not a gate.
"""

from __future__ import annotations

import argparse
import ast
import json
import re
import sys
from pathlib import Path

THRESHOLDS = {
    "strict": {
        "max_cyclomatic": 8,
        "max_nesting": 3,
        "max_function_lines": 40,
        "max_imports": 12,
        "max_classes_per_100_lines": 2.0,
        "max_file_lines": 400,
    },
    "medium": {
        "max_cyclomatic": 10,
        "max_nesting": 4,
        "max_function_lines": 50,
        "max_imports": 18,
        "max_classes_per_100_lines": 3.0,
        "max_file_lines": 600,
    },
    "relaxed": {
        "max_cyclomatic": 15,
        "max_nesting": 5,
        "max_function_lines": 80,
        "max_imports": 30,
        "max_classes_per_100_lines": 5.0,
        "max_file_lines": 1000,
    },
}

SKIP_DIRS = {"node_modules", ".git", "__pycache__", ".venv", "venv", "dist", "build", ".mypy_cache"}

# Branch nodes that each add one decision point to cyclomatic complexity.
BRANCH_NODES = (
    ast.If,
    ast.For,
    ast.AsyncFor,
    ast.While,
    ast.ExceptHandler,
    ast.With,
    ast.AsyncWith,
    ast.Assert,
    ast.comprehension,
)
NESTING_NODES = (
    ast.If,
    ast.For,
    ast.AsyncFor,
    ast.While,
    ast.With,
    ast.AsyncWith,
    ast.Try,
    ast.FunctionDef,
    ast.AsyncFunctionDef,
)
ABC_HINTS = ("ABC", "ABCMeta", "abstractmethod", "Protocol")


def detect_lang(path: Path) -> str | None:
    ext = path.suffix.lower()
    if ext == ".py":
        return "python"
    if ext in {".ts", ".tsx", ".js", ".jsx"}:
        return "typescript"
    return None


def func_complexity(node: ast.AST) -> int:
    """Cyclomatic complexity of one function: 1 + decision points within it.

    Boolean operators add (n-1) branches each; a bare `else`/`elif` is already
    counted by its owning `If`, so we don't double-count.
    """
    score = 1
    for child in ast.walk(node):
        if isinstance(child, BRANCH_NODES):
            score += 1
        elif isinstance(child, ast.BoolOp):
            score += len(child.values) - 1
    return score


def func_nesting(node: ast.AST) -> int:
    """Maximum block-nesting depth inside a function body (the function itself = 0)."""

    def depth(n: ast.AST, current: int) -> int:
        best = current
        for child in ast.iter_child_nodes(n):
            step = current + 1 if isinstance(child, NESTING_NODES) else current
            best = max(best, depth(child, step))
        return best

    return max((depth(child, 0) for child in ast.iter_child_nodes(node)), default=0)


def check_function(fn: ast.AST, th: dict) -> list[dict]:
    """Per-function findings: length, cyclomatic complexity, nesting depth."""
    findings: list[dict] = []
    length = getattr(fn, "end_lineno", fn.lineno) - fn.lineno + 1
    if length > th["max_function_lines"]:
        findings.append(
            {
                "rule": "function-length",
                "severity": "warn",
                "line": fn.lineno,
                "message": f"'{fn.name}' is {length} lines (max {th['max_function_lines']}). Split it.",
            }
        )
    cyclo = func_complexity(fn)
    if cyclo > th["max_cyclomatic"]:
        findings.append(
            {
                "rule": "cyclomatic-complexity",
                "severity": "warn",
                "line": fn.lineno,
                "message": f"'{fn.name}' has cyclomatic complexity {cyclo} (max {th['max_cyclomatic']}). Flatten branching.",
            }
        )
    nest = func_nesting(fn)
    if nest > th["max_nesting"]:
        findings.append(
            {
                "rule": "nesting-depth",
                "severity": "warn",
                "line": fn.lineno,
                "message": f"'{fn.name}' nests {nest} levels deep (max {th['max_nesting']}). Use early returns.",
            }
        )
    return findings


def has_abc_marker(classes: list, text: str) -> bool:
    """True if any class uses an ABC/Protocol base, decorator, or abstractmethod."""
    bases = {b.id for c in classes for b in c.bases if isinstance(b, ast.Name)}
    decos = {d.id for c in classes for d in c.decorator_list if isinstance(d, ast.Name)}
    return bool((bases | decos) & set(ABC_HINTS)) or bool(re.search(r"\babstractmethod\b", text))


def check_file_shape(classes: list, imports: list, text: str, line_count: int, th: dict) -> list[dict]:
    """File-level findings: import count, class density, premature abstraction."""
    findings: list[dict] = []
    if len(imports) > th["max_imports"]:
        findings.append(
            {
                "rule": "import-count",
                "severity": "warn",
                "message": f"{len(imports)} imports (max {th['max_imports']}). High coupling?",
            }
        )
    if line_count:
        density = len(classes) / (line_count / 100)
        if density > th["max_classes_per_100_lines"]:
            findings.append(
                {
                    "rule": "class-density",
                    "severity": "warn",
                    "message": f"{len(classes)} classes in {line_count} lines ({density:.1f}/100). Premature abstraction?",
                }
            )
    if classes and line_count < 200 and has_abc_marker(classes, text):
        findings.append(
            {
                "rule": "premature-abstraction",
                "severity": "info",
                "message": "Abstract base / Protocol in a file under 200 lines. Needed yet?",
            }
        )
    return findings


def analyze_python(text: str, line_count: int, th: dict) -> list[dict]:
    try:
        tree = ast.parse(text)
    except SyntaxError as exc:
        return [{"rule": "parse-error", "severity": "info", "message": f"Could not parse: {exc.msg}"}]

    functions = [n for n in ast.walk(tree) if isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef))]
    classes = [n for n in ast.walk(tree) if isinstance(n, ast.ClassDef)]
    imports = [n for n in ast.walk(tree) if isinstance(n, (ast.Import, ast.ImportFrom))]

    findings: list[dict] = []
    for fn in functions:
        findings += check_function(fn, th)
    findings += check_file_shape(classes, imports, text, line_count, th)
    return findings


# --- TypeScript / JS heuristic fallback (regex; warn-only) ---

TS_FUNC = re.compile(
    r"^\s*(?:export\s+)?(?:async\s+)?(?:function\s+\w+|(?:const|let)\s+\w+\s*=\s*(?:async\s+)?\()", re.M
)
TS_CLASS = re.compile(r"^\s*(?:export\s+)?(?:abstract\s+)?class\s+\w+", re.M)
TS_IMPORT = re.compile(r"^\s*import\s+", re.M)


def analyze_typescript(text: str, line_count: int, th: dict) -> list[dict]:
    findings: list[dict] = []
    imports = len(TS_IMPORT.findall(text))
    if imports > th["max_imports"]:
        findings.append(
            {
                "rule": "import-count",
                "severity": "warn",
                "message": f"{imports} imports (max {th['max_imports']}). High coupling?",
            }
        )

    classes = len(TS_CLASS.findall(text))
    if line_count:
        density = classes / (line_count / 100)
        if density > th["max_classes_per_100_lines"]:
            findings.append(
                {
                    "rule": "class-density",
                    "severity": "warn",
                    "message": f"{classes} classes in {line_count} lines ({density:.1f}/100). Premature abstraction?",
                }
            )

    # Indentation-based nesting estimate (2-space unit is the TS norm).
    depths = [(len(m) - len(m.lstrip())) // 2 for m in text.splitlines() if m.strip()]
    deepest = max(depths) if depths else 0
    if deepest > th["max_nesting"]:
        findings.append(
            {
                "rule": "nesting-depth",
                "severity": "warn",
                "message": f"Indentation reaches {deepest} levels (max {th['max_nesting']}). Flatten.",
            }
        )
    return findings


def analyze_file(path: Path, th: dict) -> dict | None:
    lang = detect_lang(path)
    if not lang:
        return None
    text = path.read_text(encoding="utf-8", errors="replace")
    line_count = len(text.splitlines())

    findings: list[dict] = []
    if line_count > th["max_file_lines"]:
        findings.append(
            {
                "rule": "file-length",
                "severity": "warn",
                "message": f"{line_count} lines (max {th['max_file_lines']}). Consider splitting.",
            }
        )

    findings += analyze_python(text, line_count, th) if lang == "python" else analyze_typescript(text, line_count, th)

    score = max(0, 100 - sum(15 if f["severity"] == "warn" else 5 for f in findings))
    return {"file": str(path), "language": lang, "lines": line_count, "score": score, "findings": findings}


def collect_files(target: str, extensions: list[str]) -> list[Path]:
    root = Path(target)
    if root.is_file():
        return [root]
    files: list[Path] = []
    for ext in extensions:
        files.extend(root.rglob(f"*.{ext}"))
    return [f for f in files if not SKIP_DIRS & set(f.parts)]


def build_summary(results: list[dict], threshold: str) -> dict:
    total = sum(len(r["findings"]) for r in results)
    avg = round(sum(r["score"] for r in results) / len(results), 1) if results else 100.0
    return {
        "status": "ok",
        "threshold": threshold,
        "files_analyzed": len(results),
        "total_findings": total,
        "average_score": avg,
        "verdict": "PASS" if total == 0 else ("WARN" if avg >= 50 else "FAIL"),
        "results": results,
    }


def print_report(summary: dict) -> None:
    print(f"Complexity Check — {summary['files_analyzed']} files, threshold {summary['threshold']}")
    print(f"Average score: {summary['average_score']:.0f}/100   Findings: {summary['total_findings']}\n")
    for r in summary["results"]:
        if not r["findings"]:
            continue
        print(f"  {r['file']}  (score {r['score']}/100)")
        for f in r["findings"]:
            loc = f"  line {f['line']}" if "line" in f else ""
            print(f"    [{f['severity'].upper()}] {f['rule']}{loc}: {f['message']}")
        print()
    if summary["total_findings"] == 0:
        print("  No findings. Code looks appropriately simple.")
    print(f"Verdict: {summary['verdict']}")


def main() -> None:
    ap = argparse.ArgumentParser(
        description="Detect over-engineering in Python/TypeScript (Simplicity First).",
        epilog="Thresholds: strict (new code), medium (default), relaxed (legacy).",
    )
    ap.add_argument("targets", nargs="+", help="Files or directories to analyze")
    ap.add_argument("--threshold", choices=sorted(THRESHOLDS), default="medium")
    ap.add_argument("--ext", default="py,ts,tsx,js,jsx", help="Comma-separated extensions")
    ap.add_argument("--json", action="store_true")
    args = ap.parse_args()

    th = THRESHOLDS[args.threshold]
    extensions = [e.strip().lstrip(".") for e in args.ext.split(",") if e.strip()]
    # Merge all targets; dedupe on resolved path (a dir target may overlap a file target).
    seen: set[Path] = set()
    files: list[Path] = []
    for target in args.targets:
        for f in collect_files(target, extensions):
            key = f.resolve()
            if key not in seen:
                seen.add(key)
                files.append(f)

    if not files:
        msg = f"No files matching {extensions} under {' '.join(args.targets)}"
        print(
            json.dumps({"status": "error", "message": msg}) if args.json else f"[error] {msg}",
            file=None if args.json else sys.stderr,
        )
        sys.exit(0)

    results = [r for r in (analyze_file(f, th) for f in sorted(files)) if r]
    summary = build_summary(results, args.threshold)
    if args.json:
        print(json.dumps(summary, indent=2))
        return
    print_report(summary)


if __name__ == "__main__":
    main()
