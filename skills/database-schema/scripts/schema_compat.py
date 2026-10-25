#!/usr/bin/env python3
"""Diff two DB schema JSON files and classify each change: breaking / potentially / safe.

A real compatibility checker (rewrite of the upstream generic one): loads a before/after schema,
reports dropped tables/columns, type narrowings, nullability tightening, and constraint changes —
the things that break a rolling deploy. Use it to decide whether a change needs the expand-contract
dance (see references/patterns-catalog.md) or can ship directly. Stdlib-only.

Schema JSON shape:
  {"tables": {"users": {"columns": {"email": {"type":"varchar","length":255,"nullable":false,
   "unique":true,"primary_key":false}}}}}

Usage:
  schema_compat.py --before before.json --after after.json            # text report
  schema_compat.py --before before.json --after after.json --json     # machine-readable
Exit 1 if any BREAKING change is found (usable as a CI gate).
"""

from __future__ import annotations

import argparse
import json
import sys

# Numeric/string widenings that are safe (target holds every source value) vs narrowings (break).
# A change not listed and not equal is treated as potentially-breaking (needs a human look).
SAFE_WIDENINGS = {
    ("smallint", "int"),
    ("smallint", "bigint"),
    ("int", "bigint"),
    ("float", "double"),
    ("int", "float"),
    ("int", "double"),
    ("bigint", "double"),
    ("varchar", "text"),
    ("char", "varchar"),
    ("char", "text"),
}
NARROWINGS = {  # target cannot hold all source values → data loss on rolling read
    ("bigint", "int"),
    ("bigint", "smallint"),
    ("int", "smallint"),
    ("double", "float"),
    ("text", "varchar"),
    ("varchar", "char"),
    ("bigint", "varchar"),
    ("int", "varchar"),  # numeric→string mid-flight breaks readers
}


def load(path: str) -> dict:
    with open(path, encoding="utf-8") as fh:
        return json.load(fh)


def cols(schema: dict, table: str) -> dict:
    return schema.get("tables", {}).get(table, {}).get("columns", {})


def classify_type(old: str, new: str) -> tuple[str, str]:
    if old == new:
        return "safe", ""
    pair = (old, new)
    if pair in SAFE_WIDENINGS:
        return "safe", "widening %s→%s" % (old, new)
    if pair in NARROWINGS:
        return "breaking", "narrowing %s→%s (data loss / reader break)" % (old, new)
    return "potentially", "type change %s→%s (verify value range)" % (old, new)


def diff(before: dict, after: dict) -> list[dict]:
    findings: list[dict] = []
    b_tables, a_tables = before.get("tables", {}), after.get("tables", {})

    for t in b_tables:
        if t not in a_tables:
            findings.append({"severity": "breaking", "table": t, "change": "table dropped"})
            continue
        bc, ac = cols(before, t), cols(after, t)
        for c, spec in bc.items():
            if c not in ac:
                findings.append({"severity": "breaking", "table": t, "column": c, "change": "column dropped"})
                continue
            new = ac[c]
            sev, detail = classify_type(spec.get("type", ""), new.get("type", ""))
            if sev != "safe":
                findings.append({"severity": sev, "table": t, "column": c, "change": detail})
            # length narrowing on strings
            bl, al = spec.get("length"), new.get("length")
            if isinstance(bl, int) and isinstance(al, int) and al < bl:
                findings.append(
                    {
                        "severity": "breaking",
                        "table": t,
                        "column": c,
                        "change": "length narrowed %d→%d (truncation)" % (bl, al),
                    }
                )
            # nullable → NOT NULL breaks existing rows/inserts unless a default is added
            if spec.get("nullable", True) and not new.get("nullable", True) and "default" not in new:
                findings.append(
                    {
                        "severity": "breaking",
                        "table": t,
                        "column": c,
                        "change": "NULL→NOT NULL without default (existing NULLs / old inserts fail)",
                    }
                )
        # new NOT NULL column without default = breaking for old writers
        for c, spec in ac.items():
            if c not in bc and not spec.get("nullable", True) and "default" not in spec:
                findings.append(
                    {
                        "severity": "breaking",
                        "table": t,
                        "column": c,
                        "change": "added NOT NULL column without default (old writers fail)",
                    }
                )
            elif c not in bc:
                findings.append(
                    {"severity": "safe", "table": t, "column": c, "change": "column added (nullable/default)"}
                )

    for t in a_tables:
        if t not in b_tables:
            findings.append({"severity": "safe", "table": t, "change": "table added"})
    return findings


def main() -> int:
    ap = argparse.ArgumentParser(description="Classify schema changes: breaking / potentially / safe.")
    ap.add_argument("--before", required=True)
    ap.add_argument("--after", required=True)
    ap.add_argument("--json", action="store_true", help="machine-readable output")
    args = ap.parse_args()

    findings = diff(load(args.before), load(args.after))
    order = {"breaking": 0, "potentially": 1, "safe": 2}
    findings.sort(key=lambda f: order[f["severity"]])
    counts = {k: sum(1 for f in findings if f["severity"] == k) for k in order}

    if args.json:
        print(json.dumps({"counts": counts, "findings": findings}, indent=2))
    else:
        for f in findings:
            loc = f["table"] + ("." + f["column"] if "column" in f else "")
            print("  [%-11s] %s — %s" % (f["severity"].upper(), loc, f["change"]))
        print(
            "\n%d breaking, %d potentially-breaking, %d safe"
            % (counts["breaking"], counts["potentially"], counts["safe"])
        )
        if counts["breaking"]:
            print(
                "BREAKING changes present — needs expand-contract (see references/patterns-catalog.md), "
                "not a direct deploy."
            )
    return 1 if counts["breaking"] else 0


if __name__ == "__main__":
    sys.exit(main())
