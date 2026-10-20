#!/usr/bin/env python3
"""Pre-flight safety check for browser automation against an online source.

Before automating any external site, verify it's safe and permitted:
  1. robots.txt — is the target path allowed for our user-agent? what Crawl-delay?
  2. Secret hygiene — does the automation script hardcode credentials/tokens?

Exit 0 = clear to proceed; exit 1 = a blocker was found (disallowed path or hardcoded secret).
Stdlib-only. This audits FOR responsible use — it does NOT help evade detection.

Usage:
  preflight_check.py --url https://example.com/listings          # robots.txt check
  preflight_check.py --url https://example.com/ --script scrape.py  # + secret scan
  preflight_check.py --script scrape.py                          # secret scan only
"""

from __future__ import annotations

import argparse
import re
import sys
import urllib.error
import urllib.request
from urllib.parse import urlparse
from urllib.robotparser import RobotFileParser

DEFAULT_UA = "digitalbtd-automation"

# Hardcoded-secret signatures (name = pattern). Conservative: flags obvious literals, not env reads.
SECRET_PATTERNS = {
    "password literal": re.compile(r"""(?i)\bpass(word|wd)?\s*=\s*['"][^'"]{3,}['"]"""),
    "api key / token literal": re.compile(r"""(?i)\b(api[_-]?key|token|secret|auth)\s*=\s*['"][^'"]{8,}['"]"""),
    "AWS access key": re.compile(r"\bAKIA[0-9A-Z]{16}\b"),
    "private key block": re.compile(r"-----BEGIN (?:RSA |EC )?PRIVATE KEY-----"),
    "bearer literal": re.compile(r"""(?i)bearer\s+['"]?[A-Za-z0-9._-]{20,}"""),
}
# Env-based reads are the CORRECT pattern — never flag these.
SECRET_OK = re.compile(r"""os\.(environ|getenv)|getpass|\binput\(|--\w|args\.""")


def check_robots(url: str, ua: str) -> tuple[bool, str]:
    """(allowed, detail). Fail-CLOSED on fetch error is too strict for a helper — we fail-OPEN
    with a loud warning so a transient proxy/cert error doesn't block, but the operator is told."""
    parsed = urlparse(url)
    robots_url = "%s://%s/robots.txt" % (parsed.scheme, parsed.netloc)
    rp = RobotFileParser()
    rp.set_url(robots_url)
    try:
        req = urllib.request.Request(robots_url, headers={"User-Agent": ua})
        with urllib.request.urlopen(req, timeout=15) as resp:  # noqa: S310 — fixed https target
            rp.parse(resp.read().decode("utf-8", errors="replace").splitlines())
    except (urllib.error.URLError, OSError) as exc:
        return True, "WARN: could not fetch %s (%s) — proceeding, but verify manually" % (robots_url, exc)
    allowed = rp.can_fetch(ua, url)
    delay = rp.crawl_delay(ua)
    detail = "robots.txt: %s for %s" % ("ALLOWED" if allowed else "DISALLOWED", url)
    if delay:
        detail += " | Crawl-delay=%ss (respect it — throttle between requests)" % delay
    return allowed, detail


def scan_secrets(path: str) -> list[str]:
    """Return a list of hardcoded-secret findings in the automation script."""
    findings: list[str] = []
    try:
        with open(path, encoding="utf-8") as fh:
            lines = fh.readlines()
    except OSError as exc:
        return ["ERROR: cannot read %s (%s)" % (path, exc)]
    for n, line in enumerate(lines, 1):
        if SECRET_OK.search(line):
            continue  # env var / arg / prompt — the correct pattern
        for label, pat in SECRET_PATTERNS.items():
            if pat.search(line):
                findings.append("  L%d: %s — %s" % (n, label, line.strip()[:80]))
    return findings


def main() -> int:
    ap = argparse.ArgumentParser(description="Pre-flight safety check for browser automation.")
    ap.add_argument("--url", help="Target URL to check against robots.txt")
    ap.add_argument("--script", help="Automation script to scan for hardcoded secrets")
    ap.add_argument("--user-agent", default=DEFAULT_UA, help="UA to evaluate robots.txt against")
    args = ap.parse_args()

    if not args.url and not args.script:
        ap.error("provide --url and/or --script")

    blocked = False
    if args.url:
        allowed, detail = check_robots(args.url, args.user_agent)
        print(detail)
        if not allowed:
            print("BLOCKER: robots.txt disallows this path for UA '%s'. Do not scrape it." % args.user_agent)
            blocked = True

    if args.script:
        findings = scan_secrets(args.script)
        if findings:
            print("BLOCKER: hardcoded secrets in %s — move to env vars / .env (gitignored):" % args.script)
            print("\n".join(findings))
            blocked = True
        else:
            print("secret scan: clean (%s)" % args.script)

    print("\n%s" % ("FAILED — resolve blockers above." if blocked else "OK — clear to proceed."))
    return 1 if blocked else 0


if __name__ == "__main__":
    sys.exit(main())
