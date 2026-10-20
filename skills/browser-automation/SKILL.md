---
name: browser-automation
description: Use when automating browser tasks against a site you own or are authorized to use — scraping structured data, filling forms, capturing screenshots/PDFs, extracting from SPAs, or building repeatable web-automation pipelines with Playwright. NOT for E2E testing (use a testing skill). Triggers "scrape this site", "automate this form", "screenshot this page", "extract data from", "playwright script".
---

# Browser Automation

Production browser automation with **Playwright** (Python, async): data extraction, form
filling, screenshot/PDF capture, and session management. Covers *authorized* automation of
sites you own or have permission to use — not evading a third party's bot defenses.

## Pre-flight safety gate (run FIRST, every time)

Before automating any external URL, clear these — this is the "security check on online
sources" and it is not optional:

1. **Authorization** — Is this a site we own, an internal BI system, or one we have explicit
   permission / a public API to use? If it's a third party and you're unsure, **ask the user**
   before proceeding (per `bi-policies`). Automating a site *against* its wishes can breach its
   ToS and, at scale, the law.
2. **robots.txt + rate limits** — Run `scripts/preflight_check.py --url <target>`. If it reports
   DISALLOWED, do not scrape that path. Respect any `Crawl-delay`; default to 1–3s between
   requests even when none is set.
3. **Secret hygiene** — If the script authenticates, run `preflight_check.py --script <file>`.
   Credentials come from env vars / `.env` (gitignored) / a secrets manager — **never** hardcoded.
4. **Data sensitivity** — Do not extract PII or regulated data into unmanaged files. Behind
   Zscaler/PKI, external calls may hit cert errors that aren't your bug (see `bi-policies`).

```bash
python3 scripts/preflight_check.py --url https://example.com/listings --script scrape.py
# exit 0 = clear; exit 1 = blocker (disallowed path or hardcoded secret)
```

> **Not in scope: detection evasion.** This skill deliberately excludes fingerprint spoofing,
> `navigator.webdriver` masking, and proxy-rotation-to-defeat-anti-bot. If a site actively
> blocks automation, that's a signal to stop and seek an API or permission — not to evade.

## When to use / not use

**Use for:** scraping tables/listings/search results, multi-step form workflows (login → fill →
download), screenshots/PDFs, SPA/JS-heavy extraction, repeatable data pipelines.

**Not for:** E2E test suites (use the `testing` skill); hitting a public API (do that directly —
check the package registries / the site's own network calls for one; it's faster, more reliable,
and lower-impact than scraping).

## Setup

Playwright is not installed by default here. Install on demand:

```bash
pip install playwright
playwright install chromium   # only needed for the bundled-chromium fallback below
```

**Browser channel — Edge by default.** Launches use the locally installed Microsoft Edge
(`channel="msedge"`, Chromium-based) unless overridden via `BROWSER_CHANNEL`:

```python
import os

def launch_kwargs() -> dict:
    # BROWSER_CHANNEL: msedge (default) | chrome | msedge-beta | chromium (bundled fallback)
    channel = os.environ.get("BROWSER_CHANNEL", "msedge")
    kwargs = {"headless": os.environ.get("HEADLESS", "true") == "true"}
    if channel != "chromium":
        kwargs["channel"] = channel
    return kwargs
```

Set `BROWSER_CHANNEL=chromium` where Edge isn't installed (CI, remote containers, this
harness's cloud sessions) — that omits `channel` and uses Playwright's bundled Chromium.
`headless=True` in production/CI; `headless=False` only for local debugging.

## Core competencies

- **Selector priority** (most→least reliable): `data-*` attributes → `#id` → semantic tags
  (`article`/`nav`/`main`) → classes → positional `nth-child` (last resort). XPath only for
  relationships CSS can't express. See `references/data-extraction.md`.
- **Data extraction** — tables→JSON, listings→arrays (field-selector map), nested/threaded data,
  pagination (next-button / URL `?page=N` / infinite scroll / load-more), price parsing, output
  as JSON/CSV/JSONL. Full recipes: `references/data-extraction.md`.
- **Sessions** — `context.storage_state(path=...)` to save cookies+localStorage after login;
  `new_context(storage_state=...)` to restore. Validate the session before a long job.
- **Screenshots/PDF** — `page.screenshot(full_page=True)`, element `.screenshot()`,
  `page.pdf(...)` (Chromium only).
- **Dynamic content** — wait on selectors/responses, not `sleep`; `>>` pierces open Shadow DOM;
  `scroll_into_view_if_needed()` triggers lazy loads.
- Full API: `references/playwright-api.md` (launch/context, navigation, waits, interaction,
  network interception, downloads).

## Example — single-page extraction (complete, runnable)

```python
import asyncio
from playwright.async_api import async_playwright

async def extract(url: str, container: str, fields: dict[str, str]) -> list[dict]:
    async with async_playwright() as p:
        browser = await p.chromium.launch(**launch_kwargs())  # Edge default; see Setup
        try:
            page = await (await browser.new_context(
                viewport={"width": 1920, "height": 1080},
            )).new_page()
            await page.goto(url, wait_until="networkidle")
            await page.wait_for_selector(container)          # wait on content, not a timer
            rows = []
            for card in await page.locator(container).all():
                row = {}
                for key, sel in fields.items():
                    loc = card.locator(sel)
                    row[key] = (await loc.text_content() or "").strip() if await loc.count() else None
                rows.append(row)
            return rows
        finally:
            await browser.close()                            # always clean up

asyncio.run(extract("https://example.com/products", ".product-card",
                     {"name": ".title", "price": ".price"}))
```

## Quick reference

| Task | API |
|------|-----|
| Wait for content | `await page.wait_for_selector(sel)` (never `wait_for_timeout`) |
| Wait for API call | `async with page.expect_response("**/api/*"): ...` |
| Save/restore login | `context.storage_state(path=)` / `new_context(storage_state=)` |
| Download a file | `async with page.expect_download() as d: ...; await d.value.save_as(p)` |
| Full-page screenshot | `await page.screenshot(path=, full_page=True)` |
| Polite delay | `random.uniform(1, 3)` between requests; honor `Crawl-delay` |
| Pick the browser | `BROWSER_CHANNEL=msedge` (default) / `chrome` / `chromium` (bundled fallback) |

## Common mistakes

- **Hardcoded waits** (`wait_for_timeout(5000)`) — flaky + slow. Wait on selector/URL/response.
- **No cleanup** — always `try/finally: await browser.close()` (or async context manager).
- **Ignoring robots.txt / rate limits** — run the pre-flight gate; respect `Crawl-delay`.
- **Hardcoded credentials** — env vars / `.env` only; the pre-flight secret scan blocks this.
- **Scraping when an API exists** — hit the API instead (faster, lower-impact).
- **Fragile selectors** — auto-generated classes (`.css-1a2b3c`) or deep `div>div>span` break;
  prefer `data-*` / semantic / text locators.

## Additional resources

- `references/playwright-api.md` — launch, navigation, waits, interaction, downloads, network
- `references/data-extraction.md` — extraction recipes, pagination, cleaning, output formats
- `scripts/preflight_check.py` — robots.txt + secret-hygiene gate (run before automating)
