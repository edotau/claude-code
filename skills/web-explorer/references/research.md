# Research

Use when the answer must be built from many sources: a market, policy or technology analysis, a comparison across
criteria, a literature review. A single fact, even with many clues, stays a plain search (see `SKILL.md`).

## Steps

1. Ask only when the answer would change the research: a term with two meanings, an assumed place, a name shared by
   two entities. Otherwise start.
2. Split into non-overlapping subtopics that together cover the question, carrying the user's limits (dates, places,
   exclusions) into each. Use 3 when unsure, 4 for a comparison with separate criteria, 5 for separate entities,
   regions or domains, more only for a large set.
3. Research every subtopic (see Researcher). Subagents are optional: with them, give each the subtopic, key
   questions, the user's limits and a notes-file path, then run them in the same turn. Alone, run each subtopic's
   searches and extracts yourself and keep findings with URLs in context or one scratchpad notes file.
4. Before writing, run another round for any part that would leave its section thin.
5. Write once (see Report). Save to a file only if the user asked, then give the path after the report. If a report
   on the subject already exists in this conversation, research only what is new or needs rechecking.

## Researcher

Cover only your subtopic. The notes are the only material the report uses, so every usable finding needs its URL.

- Name the gaps between you and the key questions, search them, then extract the pages you expect to cite. A snippet
  is a lead; a number, date or quote needs the page.
- Prefer the authoritative source (the company's site, the paper, the filing, the dataset, official docs, a
  regulator, a standards body) and cite that page. Use another source for a fact the authoritative page lacks, and
  say which is which.
- Search again with different words when a query is thin; for a recent topic trust what you just retrieved over
  training data.
- About eight searches and extracts per subtopic; stop by twelve, or earlier when the key questions have sourced
  answers or new searches return pages you already have.
- Write a finding only if you can point at its page. Two sources disagreeing: record both, authoritative first, and
  the disagreement. No reliable source: record a gap, which is a useful result. Leave out bare predictions, unnamed
  sources and figures that appear only on aggregators, unless labeled so.

Notes file, one section per key question and no other top-level sections:

```markdown
# {Subtopic}

## {Key question}

### Takeaway
{One or two sentences.}

### Cited findings
- {Fact} - [Authoritative source](URL)
- {Fact} - [Authoritative source](URL); contradicted by [Other source](URL)

### Inferences
- {What the findings support beyond what any one page states.}

### Gaps
- {What you could not answer, and what you tried.}
```

## Report

Write from the notes only; do not search again. A claim with no URL in the notes does not go in. Where new notes
disagree with an earlier report, the new notes win, and the report stands on its own.

```markdown
# {Title: about six words, with a verb, stating the finding}

{One paragraph that answers the question, then the stakes and context. Someone who stops here knows the answer.}

## {Signpost heading that states a specific finding}
{Prose. **Bold** the figures and names a reader would scan for. Cite checkable claims as ([Source](URL)).}

## Conclusion
{One or two paragraphs on what the findings imply. Nothing restated from above.}
```

- One section per part of the question; each named entity, region or mechanism gets a subsection with its figures.
- Headings in sentence case that state the result, so the headings alone give the argument: "Usage pricing costs more
  past 50 seats", not "Pricing".
- Use a table when several items share the same fields. Cite figures, dates and attributed positions inline.
- When sources disagree, say so, name the one you take and why; state what nobody confirmed. Take the position the
  evidence supports instead of padding with "may" or "possibly".
