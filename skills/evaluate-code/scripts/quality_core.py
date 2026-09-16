#!/usr/bin/env python3
"""
quality_core.py — shared code-quality analysis primitives.

The language-agnostic core behind `code_quality_checker.py`: language detection,
line counting, regex-based function/class discovery, code-smell + SOLID-violation
detection, and the 0-100 quality score. Importable (no CLI); the thin CLI lives in
`code_quality_checker.py`.

Heuristic by design — regex function/class discovery and keyword-counted cyclomatic
complexity are adequate for an advisory signal across many languages. For precise
PER-FUNCTION Python metrics, use `complexity_checker.py` (AST-based) instead.
"""

from __future__ import annotations

import re
from pathlib import Path

# Language-specific file extensions
LANGUAGE_EXTENSIONS = {
    "python": [".py"],
    "typescript": [".ts", ".tsx"],
    "javascript": [".js", ".jsx", ".mjs"],
    "go": [".go"],
    "swift": [".swift"],
    "kotlin": [".kt", ".kts"],
}

# Code smell thresholds
THRESHOLDS = {
    "long_function_lines": 50,
    "too_many_parameters": 5,
    "high_complexity": 10,
    "god_class_methods": 20,
    "max_imports": 15,
}

# Language-specific patterns, grouped so each language's rules sit together.
FUNCTION_PATTERNS = {
    "python": r"def\s+(\w+)\s*\(([^)]*)\)",
    "typescript": r"(?:function\s+(\w+)|(?:const|let|var)\s+(\w+)\s*=\s*(?:async\s+)?\([^)]*\)\s*=>)",
    "javascript": r"(?:function\s+(\w+)|(?:const|let|var)\s+(\w+)\s*=\s*(?:async\s+)?\([^)]*\)\s*=>)",
    "go": r"func\s+(?:\([^)]+\)\s+)?(\w+)\s*\(([^)]*)\)",
    "swift": r"func\s+(\w+)\s*\(([^)]*)\)",
    "kotlin": r"fun\s+(\w+)\s*\(([^)]*)\)",
}

CLASS_PATTERNS = {
    "python": r"class\s+(\w+)",
    "typescript": r"class\s+(\w+)",
    "javascript": r"class\s+(\w+)",
    "go": r"type\s+(\w+)\s+struct",
    "swift": r"class\s+(\w+)",
    "kotlin": r"class\s+(\w+)",
}

METHOD_PATTERNS = {
    "python": r"def\s+\w+\s*\(",
    "typescript": r"(?:public|private|protected)?\s*\w+\s*\([^)]*\)\s*[:{]",
    "javascript": r"\w+\s*\([^)]*\)\s*\{",
    "go": r"func\s+\(",
    "swift": r"func\s+\w+",
    "kotlin": r"fun\s+\w+",
}


def detect_language(filepath: Path) -> str | None:
    """Detect programming language from file extension, or None if unsupported."""
    ext = filepath.suffix.lower()
    for lang, extensions in LANGUAGE_EXTENSIONS.items():
        if ext in extensions:
            return lang
    return None


def read_file_content(filepath: Path) -> str:
    """Read file content safely; empty string on any read error."""
    try:
        return filepath.read_text(encoding="utf-8", errors="ignore")
    except OSError:
        return ""


def calculate_cyclomatic_complexity(content: str) -> int:
    """Estimate cyclomatic complexity by counting control-flow keywords (regex)."""
    complexity = 1  # base path
    patterns = [
        r"\bif\b",
        r"\belif\b",
        r"\belse\b",
        r"\bfor\b",
        r"\bwhile\b",
        r"\bcase\b",
        r"\bcatch\b",
        r"\bexcept\b",
        r"\band\b",
        r"\bor\b",
        r"\|\|",
        r"&&",
    ]
    for pattern in patterns:
        complexity += len(re.findall(pattern, content, re.IGNORECASE))
    return complexity


def count_lines(content: str) -> dict[str, int]:
    """Count total / code / blank / comment lines."""
    lines = content.split("\n")
    total = len(lines)
    blank = sum(1 for line in lines if not line.strip())
    comment = 0
    for line in lines:
        stripped = line.strip()
        if stripped.startswith(("#", "//", "/*", "'''", '"""')):
            comment += 1
    code = total - blank - comment
    return {"total": total, "code": code, "blank": blank, "comment": comment}


def find_functions(content: str, language: str) -> list[dict]:
    """Find function definitions with parameter count, length, and complexity."""
    functions: list[dict] = []
    pattern = FUNCTION_PATTERNS.get(language, FUNCTION_PATTERNS["python"])
    matches = re.finditer(pattern, content, re.MULTILINE)

    for match in matches:
        name = next((g for g in match.groups() if g), "anonymous")
        params_str = match.group(2) if len(match.groups()) > 1 and match.group(2) else ""
        params = [p.strip() for p in params_str.split(",") if p.strip()]

        # Estimate function length by spanning to the next function definition.
        remaining = content[match.end() :]
        next_func = re.search(pattern, remaining)
        func_body = remaining[: next_func.start()] if next_func else remaining[: min(2000, len(remaining))]

        functions.append(
            {
                "name": name,
                "parameters": len(params),
                "lines": len(func_body.split("\n")),
                "complexity": calculate_cyclomatic_complexity(func_body),
            }
        )
    return functions


def find_classes(content: str, language: str) -> list[dict]:
    """Find class definitions with method count and length."""
    classes: list[dict] = []
    pattern = CLASS_PATTERNS.get(language, CLASS_PATTERNS["python"])
    method_pattern = METHOD_PATTERNS.get(language, METHOD_PATTERNS["python"])

    for match in re.finditer(pattern, content):
        remaining = content[match.end() :]
        next_class = re.search(pattern, remaining)
        class_body = remaining[: next_class.start()] if next_class else remaining
        classes.append(
            {
                "name": match.group(1),
                "methods": len(re.findall(method_pattern, class_body)),
                "lines": len(class_body.split("\n")),
            }
        )
    return classes


def check_code_smells(content: str, functions: list[dict], classes: list[dict]) -> list[dict]:
    """Flag long functions, excess parameters, high complexity, god classes, magic numbers, commented code."""
    smells: list[dict] = []

    for func in functions:
        if func["lines"] > THRESHOLDS["long_function_lines"]:
            smells.append(
                {
                    "type": "long_function",
                    "severity": "medium",
                    "message": f"Function '{func['name']}' has {func['lines']} lines (max: {THRESHOLDS['long_function_lines']})",
                    "location": func["name"],
                }
            )
        if func["parameters"] > THRESHOLDS["too_many_parameters"]:
            smells.append(
                {
                    "type": "too_many_parameters",
                    "severity": "low",
                    "message": f"Function '{func['name']}' has {func['parameters']} parameters (max: {THRESHOLDS['too_many_parameters']})",
                    "location": func["name"],
                }
            )
        if func["complexity"] > THRESHOLDS["high_complexity"]:
            severity = "high" if func["complexity"] > 20 else "medium"
            smells.append(
                {
                    "type": "high_complexity",
                    "severity": severity,
                    "message": f"Function '{func['name']}' has complexity {func['complexity']} (max: {THRESHOLDS['high_complexity']})",
                    "location": func["name"],
                }
            )

    for cls in classes:
        if cls["methods"] > THRESHOLDS["god_class_methods"]:
            smells.append(
                {
                    "type": "god_class",
                    "severity": "high",
                    "message": f"Class '{cls['name']}' has {cls['methods']} methods (max: {THRESHOLDS['god_class_methods']})",
                    "location": cls["name"],
                }
            )

    magic_pattern = r"\b(?<![.\"\'])\d{3,}\b(?!\.\d)"
    commented_code_pattern = r"^\s*[#//]+\s*(if|for|while|def|function|class|const|let|var)\s"
    for i, line in enumerate(content.split("\n"), 1):
        if not line.strip().startswith(("#", "//", "import", "from")):
            for match in re.findall(magic_pattern, line)[:1]:  # one per line
                smells.append(
                    {
                        "type": "magic_number",
                        "severity": "low",
                        "message": f"Magic number {match} should be a named constant",
                        "location": f"line {i}",
                    }
                )
        if re.match(commented_code_pattern, line, re.IGNORECASE):
            smells.append(
                {
                    "type": "commented_code",
                    "severity": "low",
                    "message": "Commented-out code should be removed",
                    "location": f"line {i}",
                }
            )
    return smells


def check_solid_violations(content: str) -> list[dict]:
    """Flag potential OCP (type-checking), LSP/ISP (NotImplementedError), DIP (import-heavy) violations."""
    violations: list[dict] = []

    type_checks = len(re.findall(r"isinstance\(|type\(.*\)\s*==|typeof\s+\w+\s*===", content))
    if type_checks > 2:
        violations.append(
            {
                "principle": "OCP",
                "name": "Open/Closed Principle",
                "severity": "medium",
                "message": f"Found {type_checks} type checks - consider using polymorphism",
            }
        )

    not_impl = len(re.findall(r"raise\s+NotImplementedError|not\s+implemented", content, re.IGNORECASE))
    if not_impl:
        violations.append(
            {
                "principle": "LSP/ISP",
                "name": "Liskov/Interface Segregation",
                "severity": "low",
                "message": f"Found {not_impl} unimplemented methods - may indicate oversized interface",
            }
        )

    imports = len(re.findall(r"^(?:import|from)\s+", content, re.MULTILINE))
    if imports > THRESHOLDS["max_imports"]:
        violations.append(
            {
                "principle": "DIP",
                "name": "Dependency Inversion Principle",
                "severity": "low",
                "message": f"File has {imports} imports - consider dependency injection",
            }
        )
    return violations


def calculate_quality_score(
    line_metrics: dict,
    functions: list[dict],
    classes: list[dict],
    smells: list[dict],
    violations: list[dict],
) -> int:
    """Overall quality score 0-100: deduct for smells/violations, bonus for good shape."""
    score = 100
    smell_penalty = {"high": 10, "medium": 5, "low": 2}
    violation_penalty = {"high": 8, "medium": 4, "low": 2}

    for smell in smells:
        score -= smell_penalty.get(smell["severity"], 0)
    for violation in violations:
        score -= violation_penalty.get(violation["severity"], 0)

    if line_metrics["total"] > 0:
        comment_ratio = line_metrics["comment"] / line_metrics["total"]
        if 0.1 <= comment_ratio <= 0.3:
            score += 5
    if functions:
        avg_lines = sum(f["lines"] for f in functions) / len(functions)
        if avg_lines < 30:
            score += 5

    return max(0, min(100, score))


def get_grade(score: int) -> str:
    """Convert a 0-100 score to a letter grade."""
    if score >= 90:
        return "A"
    if score >= 80:
        return "B"
    if score >= 70:
        return "C"
    if score >= 60:
        return "D"
    return "F"
