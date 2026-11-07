# Monitor

Use for "watch", "track" or "alert me when X changes". Turn the watch into a search or extraction that can run again
unchanged, run it once and save the baseline; the monitor file is the result. Nothing here schedules itself: start a
scheduler only if the user asks, and point it at the monitor folder.

For a one-time lookup use plain search; for a one-time table use `list.md`; for a report use `research.md`.

## Pick the kind

| You have | Kind | What "changed" means |
| --- | --- | --- |
| A topic, pages not known yet | search | A result URL not in the previous snapshot |
| One or more known URLs | extract | The hash of the page's extracted content changed |

Search suits mentions, news and jobs; extract suits a pricing page, changelog, docs page or status page.

## Steps

1. Write the query or URL list so the next run can reuse it verbatim. Leave dates and "latest" out of a query: the
   snapshot, not the wording, decides what is new.
2. Write the change rule in one sentence. Search: a new URL is the alert; a URL missing this run is not (a page
   that could not be fetched is left out). Extract: a changed hash is the alert, and the alert quotes the part the
   user cares about (the price, the version), not the whole page.
3. Name a cadence: the user's, else daily for news and changelogs and weekly for slow pages, and say which you chose.
4. Run it once (`claude-code network websearch --query "<q>"` for search, `--extract <url>` for extract; use
   `tvly search --time-range` when recency matters). If results are the wrong kind of thing, fix the query or URLs and
   rerun before saving anything. If the run fails, say so and leave `snapshot.json` as it was.
5. Save the monitor and snapshot. Each later run reads the monitor, repeats the search or extract, reports only what
   the change rule flags, and replaces `snapshot.json`.

A changed query or URL list is a new monitor with a new folder and baseline; editing in place makes the next diff
meaningless. Create `monitors/{name}/` in the working directory (name in sentence case, 3-6 words, letters, numbers,
single spaces; quote it in shell commands).

## Files

`monitors/{name}/monitor.md`:

```markdown
# {Name}

- Cadence: {daily | weekly on Monday | the schedule the user asked for}
- Kind: search | extract
- Query: {the query, for a search}
- URLs: {one per line, for an extract}
- Alert when: {the change rule}
- Snapshot: snapshot.json

## Run again

Read this file, run the same search or extract, compare with snapshot.json using the alert rule, report only the
changes, then replace snapshot.json with this run. If the run fails, report that and keep the snapshot.
```

`monitors/{name}/snapshot.json`:

```json
{
  "ran_at": "YYYY-MM-DD",
  "kind": "search",
  "query": "",
  "items": [{ "url": "", "title": "", "snippet": "" }]
}
```

For an extract store `pages` instead of `items`: `url`, `sha256` of the extracted content, and a short excerpt of
the watched part.

In the reply give the cadence, the query or URLs, the alert rule, the folder path, and what the baseline already shows.
