# Entity

Use to resolve one company, person or organization into a CRM record, or enrich a CSV or pasted list of known names.
Every value comes from a page you read; an empty field stays empty. New matches: `list.md`; deep report:
`research.md`.

## Identify first

1. Take the type and every identifier the user gave: domain, city, employer, title, product.
2. Search the name for other entities using it; a common name, or one that is also a product or place, is a collision
   until proven otherwise.
3. The entity is right only when two identifiers agree: company = name plus its own domain; person = name plus an
   employer, location or a profile they control. One page mentioning the name is not identification.
4. More than one entity still fits: stop and return the candidates; never pick by result count or copy a field
   across candidates.
5. Put a stable identifier (domain or employer) in every later search, not the bare name.

## Read authoritative sources

- Extract the page that owns the fact (all needed pages in one call); a snippet is not enough for a number.
- Own site, official profile, filing or registry first; for a person, the employer's team page or a profile they
  control before a people-search site.
- A directory, data broker or roundup is a lead; use it as the source only when no authoritative page states the
  fact, and say so in the source note.
- Two authoritative pages disagreeing on a number: keep the more direct one, note the other; never average.
- Email or phone only when a page shows it. Search recent news (with domain or employer) only when asked for signals.

## A CSV of known rows

Enrich those rows; do not look for new names.

1. Read and count the rows, and use the fields the user named (ask once if none).
2. Above 25 rows, state the count and fields and wait for a go-ahead.
3. Per row, follow the two sections above. A row you cannot isolate stays unresolved: blank new fields, a note on
   what collided, no guessed values.
4. Write the original columns plus the new fields, a source per new field and a status (`filled` or `unresolved`) to
   `{name}-enriched-{YYYY-MM-DD}.csv` with a CSV writer. In the reply give the filled and unresolved counts and the
   path; do not paste a record per row.

## Output

For specific questions, answer them with a source per value and skip the template. For a full record:

```markdown
# {Name}

type: company | person | organization
identified by: {domain, employer, or other stable identifier}
as_of: {YYYY-MM-DD}

| Field | Value | Source |
| --- | --- | --- |
| website | | |
| description | | |
| industry | | |
| headquarters | | |
| founded | | |
| employees | | |
| funding | | |
| revenue | | |

## Recent signals
- {YYYY-MM-DD}: {fact} - {url}

## Unknown
- {field}: {what you tried}
```

- A person's fields: title, organization, location, profile URL; add a People table (name, title, source) only when
  related people were asked for. Drop rows that do not apply to the type.
- A field that applies but is unfilled stays blank in the table, with a line under Unknown.
- Description: one or two sentences from the about page or profile. Recent signals: at most five dated items; omit
  empty sections.
- When no single entity can be picked, return a table of `Name | Website | What separates it from the others`.
