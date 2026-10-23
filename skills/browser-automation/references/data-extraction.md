# Data Extraction Recipes

Practical patterns for extracting structured data from web pages using Playwright. Each recipe is
a self-contained pattern you can adapt to your target site.

## Example — single-page extraction (complete, runnable)

Load the [Setup launch helper](playwright-api.md#setup) before running this example.

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

## Pagination Handling

### Next-Button Pagination

The most common pattern. Click "Next" until the button disappears or is disabled.

```python
async def paginate_via_next_button(page, next_selector, content_selector, max_pages=100):
    """
    Yield page objects as you paginate through results.

    next_selector: CSS selector for the "Next" button/link
    content_selector: CSS selector to wait for after navigation (confirms new page loaded)
    """
    pages_scraped = 0

    while pages_scraped < max_pages:
        yield page  # Caller extracts data from current page
        pages_scraped += 1

        next_btn = page.locator(next_selector)
        if await next_btn.count() == 0:
            break

        try:
            is_disabled = await next_btn.is_disabled()
        except Exception:
            is_disabled = True

        if is_disabled:
            break

        await next_btn.click()
        await page.wait_for_selector(content_selector, state="attached")
        await page.wait_for_timeout(random.randint(500, 1500))  # polite delay between pages
```

### URL-Based Pagination

When pages follow a predictable URL pattern:

```python
async def paginate_via_url(page, url_template, start=1, max_pages=100):
    """
    Navigate through pages using URL parameters.

    url_template: URL with {page} placeholder, e.g., "https://example.com/search?page={page}"
    """
    for page_num in range(start, start + max_pages):
        url = url_template.format(page=page_num)
        response = await page.goto(url, wait_until="networkidle")

        if response and response.status == 404:
            break

        yield page, page_num
        await page.wait_for_timeout(random.randint(800, 2500))  # polite delay between pages
```

### Infinite Scroll

For sites that load content as you scroll:

```python
async def paginate_via_scroll(page, item_selector, max_scrolls=100, no_change_limit=3):
    """
    Scroll to load more content until no new items appear.

    item_selector: CSS selector for individual items (used to count progress)
    no_change_limit: Stop after N scrolls with no new items
    """
    previous_count = 0
    no_change_streak = 0

    for scroll_num in range(max_scrolls):
        current_count = await page.locator(item_selector).count()

        if current_count == previous_count:
            no_change_streak += 1
            if no_change_streak >= no_change_limit:
                break
        else:
            no_change_streak = 0

        previous_count = current_count

        await page.evaluate("window.scrollTo(0, document.body.scrollHeight)")
        await page.wait_for_timeout(random.randint(1000, 2500))  # polite delay

        load_more = page.locator("button:has-text('Load More'), button:has-text('Show More')")
        if await load_more.count() > 0 and await load_more.is_visible():
            await load_more.click()
            await page.wait_for_timeout(random.randint(1000, 2000))

    return current_count
```

### Load-More Button

Simpler variant of infinite scroll where content loads via a button:

```python
async def paginate_via_load_more(page, button_selector, item_selector, max_clicks=50):
    """Click a 'Load More' button repeatedly until it disappears."""
    for click_num in range(max_clicks):
        btn = page.locator(button_selector)
        if await btn.count() == 0 or not await btn.is_visible():
            break

        count_before = await page.locator(item_selector).count()
        await btn.click()

        try:
            await page.wait_for_function(
                f"document.querySelectorAll('{item_selector}').length > {count_before}",
                timeout=10000,
            )
        except Exception:
            break  # No new items loaded

        await page.wait_for_timeout(random.randint(500, 1500))  # polite delay

    return await page.locator(item_selector).count()
```

## Data Cleaning Utilities

### Post-Extraction Cleaning

```python
import re

def clean_text(text):
    """Normalize whitespace, remove zero-width characters."""
    if not text:
        return None
    # Remove zero-width characters
    text = re.sub(r'[​‌‍﻿]', '', text)
    # Normalize whitespace
    text = re.sub(r'\s+', ' ', text).strip()
    return text if text else None

def clean_url(url, base_url=None):
    """Convert relative URLs to absolute."""
    if not url:
        return None
    url = url.strip()
    if url.startswith("//"):
        return "https:" + url
    if url.startswith("/") and base_url:
        return base_url.rstrip("/") + url
    return url

def deduplicate(items, key_field):
    """Remove duplicate items based on a key field."""
    seen = set()
    unique = []
    for item in items:
        key = item.get(key_field)
        if key and key not in seen:
            seen.add(key)
            unique.append(item)
    return unique
```

### Output Formats

```python
import json
import csv

def to_jsonl(items, file_path):
    """Write items as JSON Lines (one JSON object per line)."""
    with open(file_path, "w") as f:
        for item in items:
            f.write(json.dumps(item, ensure_ascii=False) + "\n")

def to_csv(items, file_path):
    """Write items as CSV."""
    if not items:
        return
    headers = list(items[0].keys())
    with open(file_path, "w", newline="") as f:
        writer = csv.DictWriter(f, fieldnames=headers)
        writer.writeheader()
        writer.writerows(items)

def to_json(items, file_path, indent=2):
    """Write items as a JSON array."""
    with open(file_path, "w") as f:
        json.dump(items, f, indent=indent, ensure_ascii=False)
```
