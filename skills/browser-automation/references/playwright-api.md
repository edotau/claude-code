# Playwright Browser API Reference (Automation Focus)

Playwright's own API needs no restating; <https://playwright.dev> is current. This file holds only
what this harness adds.

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

## Init Scripts

> Do **not** use init scripts to spoof `navigator.webdriver`, plugins, or fingerprints to
> defeat a third party's bot detection — see the Pre-flight safety gate in SKILL.md. This skill
> covers authorized automation, not evasion.
