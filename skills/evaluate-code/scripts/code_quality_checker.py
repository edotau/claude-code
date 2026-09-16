#!/usr/bin/env python3
"""
code_quality_checker.py — code smells, complexity, and SOLID violations.

Thin CLI over `quality_core.py`. Multi-language (Python/TS/JS/Go/Swift/Kotlin) via
regex heuristics. For precise per-function Python metrics use `complexity_checker.py`.

Usage:
    code_quality_checker.py path/to/file.py
    code_quality_checker.py src/ --recursive
    code_quality_checker.py . --language typescript --json

Exit code is 0 except when the target path does not exist.
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

# Append (not insert(0)) so a sibling file can't shadow a stdlib module of the same name.
sys.path.append(str(Path(__file__).resolve().parent))
from quality_core import (
    LANGUAGE_EXTENSIONS,
    calculate_quality_score,
    check_code_smells,
    check_solid_violations,
    count_lines,
    detect_language,
    find_classes,
    find_functions,
    get_grade,
    read_file_content,
)

SKIP_PARTS = {"node_modules", ".git", "__pycache__", ".venv", "venv", "dist", "build"}


def analyze_file(filepath: Path) -> dict:
    """Analyze a single file for code quality."""
    language = detect_language(filepath)
    if not language:
        return {"error": f"Unsupported file type: {filepath.suffix}"}

    content = read_file_content(filepath)
    if not content:
        return {"error": f"Could not read file: {filepath}"}

    line_metrics = count_lines(content)
    functions = find_functions(content, language)
    classes = find_classes(content, language)
    smells = check_code_smells(content, functions, classes)
    violations = check_solid_violations(content)
    score = calculate_quality_score(line_metrics, functions, classes, smells, violations)

    return {
        "file": str(filepath),
        "language": language,
        "metrics": {
            "lines": line_metrics,
            "functions": len(functions),
            "classes": len(classes),
            "avg_complexity": round(sum(f["complexity"] for f in functions) / max(1, len(functions)), 1),
        },
        "quality_score": score,
        "grade": get_grade(score),
        "smells": smells,
        "solid_violations": violations,
        "function_details": functions[:10],
        "class_details": classes[:10],
    }


def analyze_directory(dir_path: Path, recursive: bool = True, language: str | None = None) -> dict:
    """Analyze all supported files in a directory."""
    if language:
        extensions = LANGUAGE_EXTENSIONS.get(language, [])
    else:
        extensions = [ext for exts in LANGUAGE_EXTENSIONS.values() for ext in exts]

    pattern = "**/*" if recursive else "*"
    results = []
    for ext in extensions:
        for filepath in dir_path.glob(f"{pattern}{ext}"):
            if SKIP_PARTS & set(filepath.parts):
                continue
            result = analyze_file(filepath)
            if "error" not in result:
                results.append(result)

    if not results:
        return {"error": "No supported files found"}

    avg_score = sum(r["quality_score"] for r in results) / len(results)
    return {
        "directory": str(dir_path),
        "files_analyzed": len(results),
        "average_score": round(avg_score, 1),
        "overall_grade": get_grade(int(avg_score)),
        "total_code_smells": sum(len(r["smells"]) for r in results),
        "total_solid_violations": sum(len(r["solid_violations"]) for r in results),
        "files": sorted(results, key=lambda x: x["quality_score"]),
    }


def print_report(analysis: dict) -> None:
    """Print a human-readable analysis report."""
    if "error" in analysis:
        print(f"Error: {analysis['error']}")
        return

    print("=" * 60)
    print("CODE QUALITY REPORT")
    print("=" * 60)

    if "file" in analysis:
        print(f"\nFile: {analysis['file']}")
        print(f"Language: {analysis['language']}")
        print(f"Quality Score: {analysis['quality_score']}/100 ({analysis['grade']})")
        metrics = analysis["metrics"]
        print(
            f"\nLines: {metrics['lines']['total']} "
            f"({metrics['lines']['code']} code, {metrics['lines']['comment']} comments)"
        )
        print(f"Functions: {metrics['functions']}   Classes: {metrics['classes']}")
        print(f"Avg Complexity: {metrics['avg_complexity']}")
        if analysis["smells"]:
            print("\n--- CODE SMELLS ---")
            for smell in analysis["smells"][:10]:
                print(f"  [{smell['severity'].upper()}] {smell['message']} ({smell['location']})")
        if analysis["solid_violations"]:
            print("\n--- SOLID VIOLATIONS ---")
            for v in analysis["solid_violations"]:
                print(f"  [{v['principle']}] {v['message']}")
    else:
        print(f"\nDirectory: {analysis['directory']}")
        print(f"Files Analyzed: {analysis['files_analyzed']}")
        print(f"Average Score: {analysis['average_score']}/100 ({analysis['overall_grade']})")
        print(f"Total Code Smells: {analysis['total_code_smells']}")
        print(f"Total SOLID Violations: {analysis['total_solid_violations']}")
        print("\n--- FILES BY QUALITY ---")
        for f in analysis["files"][:10]:
            print(f"  {f['quality_score']:3d}/100 [{f['grade']}] {f['file']}")

    print("\n" + "=" * 60)


def main() -> None:
    parser = argparse.ArgumentParser(description="Analyze code quality, smells, and SOLID violations")
    parser.add_argument("path", help="File or directory to analyze")
    parser.add_argument(
        "--recursive", "-r", action="store_true", default=True, help="Recurse into directories (default)"
    )
    parser.add_argument("--language", "-l", choices=list(LANGUAGE_EXTENSIONS.keys()), help="Filter by language")
    parser.add_argument("--json", action="store_true", help="Output JSON")
    parser.add_argument("--output", "-o", help="Write output to file")
    args = parser.parse_args()

    target = Path(args.path).resolve()
    if not target.exists():
        print(f"Error: Path does not exist: {target}", file=sys.stderr)
        sys.exit(1)

    analysis = analyze_file(target) if target.is_file() else analyze_directory(target, args.recursive, args.language)

    if args.json:
        output = json.dumps(analysis, indent=2, default=str)
        if args.output:
            Path(args.output).write_text(output, encoding="utf-8")
            print(f"Results written to {args.output}")
        else:
            print(output)
    else:
        print_report(analysis)


if __name__ == "__main__":
    main()
