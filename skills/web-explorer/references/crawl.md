# Map and crawl

Map lists URLs on a site without reading them; crawl follows links and returns page content. Both need
`TAVILY_API_KEY`: run `eval "$(claude-code vault get --export TAVILY_API_KEY)"` in the same shell command.

## Map first

- Use it when the site is known but the page is not: map with `--instructions` describing the page, then extract
  the one or two matches. This costs far less than crawling to find a single page.
- Use it to learn the structure before a crawl; the paths tell you what to put in `--select-paths`.
- It starts one level deep. Raise `--max-depth` only when the page is buried, and keep a `--limit`. If the site has
  a docs or blog subdomain, map that instead of the home page.

```bash
tvly map https://go.dev/doc/ --instructions 'tutorials' --limit 20 --select-paths '/doc/.*'
```

## Crawl focused and small

- Always set `--limit` (about 20 to start) and `--instructions`; an unfocused crawl returns every page it reaches
  and floods your context, and without a limit it runs long and costs more.
- Start at `--max-depth 1`; each extra level multiplies pages and time. Widen only if needed pages are missing.
- `--chunks-per-source` (1-5, requires `--instructions`) returns the relevant parts of each page, not whole pages.
- Aim at the section with `--select-paths` (comma-separated regexes such as `/docs/.*`) and drop noise with
  `--exclude-paths '/blog/,/archive/'`. `--no-external` is the default; keep it.

```bash
tvly crawl https://go.dev/doc/ --max-depth 1 --limit 20 --select-paths '/doc/.*' \
  --instructions 'install instructions' --chunks-per-source 1 --output-dir /tmp/scratch/crawl
```

## Read or save

- To answer a question: keep the crawl focused and chunked, and read the result selectively.
- To keep a copy of a section: omit `--chunks-per-source` and use `--output-dir`, which writes one `.md` per page;
  read those files selectively instead of printing the crawl. `-o file` saves the whole JSON response.
- Check `failed_results` after every crawl; retry those pages with `tvly extract --extract-depth advanced` and tell
  the user about any section that could not be read.
- Quote every URL in the shell: `?` and `&` are special characters.
