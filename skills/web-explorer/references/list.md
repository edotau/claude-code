# List

Use when the user wants a deduplicated table of matches for a set ("all X", "a list of Y"). The table is what the
search returned, never every match that exists: do not call it complete or collapse rows into a summary.

For one record or a CSV of known rows use `entity.md`; for a deep report use `research.md`.

## Steps

1. Before any search, fix the inclusion rule in one sentence, the columns and a cap. Columns always include name,
   website or profile, a one-line description and the source URL, plus any the user named. If extra columns or a cap
   were not given, ask once and wait; for an unbounded set ("every restaurant") the question includes the boundary.
2. Search from several angles: the category, "list of" roundups, directories, names seen on pages already opened.
   One query does not cover the set. Pull every name off a page that is itself a list.
3. Give each row an identity: an organization's website host without `www`; a person's profile URL, or name plus
   organization; a product on a shared host, its canonical URL.
4. Merge rows with the same identity, keep the fuller one and record the folded names. A match with no website gets
   one search; if still none, keep the row with the website blank.
5. Stop at the cap, or when two new angles in a row add no rows, and say which. Neither means the set is complete.
6. For more, search again excluding every name and website already kept, append new rows, merge again, and raise the
   cap by the requested amount.

Use the same columns on every row; leave a cell blank when no page states it.

## Output

Show the table in the reply and save the same markdown to `lists/{title}.md` in the working directory (title in
sentence case, 3-6 words, letters, numbers and single spaces).

```markdown
# {Title}

Inclusion: {one sentence}
As of: {YYYY-MM-DD}
Cap: {N}
Stop: {two angles added nothing | hit the cap}
Rows: {count after dedup}
Coverage: not exhaustive. This is what the angles below returned.

| Name | Website | Description | Source |
| --- | --- | --- | --- |

## Merged
- {kept name} <- {other names} ({shared website})

## Coverage
Angles: {the queries and the list pages you opened}
Excluded this pass: {names and websites already kept, or none}
Likely gaps: {the part of the set this pass could not see}
```
