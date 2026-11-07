# Entity

Use to resolve one company, person or organization and return a CRM record, or to enrich a CSV or pasted list of
known names. The name is not the identity. Every value comes from a page you read; an empty field stays empty.

To discover new matches use `list.md`; for a deep report use `research.md`.

## Identify first

People and companies share names, and a record mixing two is worse than an empty one.

1. Take the type and every identifier the user gave: domain, city, employer, title, product.
2. Search the name and look for other entities using it. A common name, or one that is also a product or place, is a
   collision until proven otherwise.
3. You have the right entity only when two identifiers agree: for a company, the name plus its own domain; for a
   person, the name plus an employer, location or a profile they control whose organization matches. One page that
   mentions the name is not identification.
4. If more than one entity still fits, stop and return the candidates. Do not pick the one with more results or copy
   a field across candidates.
5. Put a stable identifier (domain or employer) into every later search, not the bare name.

## Read authoritative sources

- Extract the page that owns the fact; a snippet is not enough for a number. Extract the pages for all needed fields
  in one call.
- The entity's own site, an official profile, a filing or a registry come first. For a person, the employer's team
  page or a profile they control comes before a people-search site.
- A directory, data broker or roundup is a lead to the owning page. Use it as the source only when no authoritative
  page states the fact, and say so in the source note.
- Two authoritative pages disagreeing on a number: keep the more direct one and note the other. Do not average.
- Include an email or phone only when a page shows it.
- Search recent news, with the domain or employer in the query, only when the user wants recent signals.

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

- For a person the fields are title, organization, location and profile URL; add a People table
  (name, title, source) only when related people were asked for. Drop rows that do not apply to the type.
- A field that applies but could not be filled stays in the table, blank, with a line under Unknown.
- Description is one or two sentences grounded in the about page or profile. Recent signals: at most five dated
  items (funding, launch, leadership change, hiring); omit empty sections.
- When no single entity can be picked, return a table of `Name | Website | What separates it from the others`.
