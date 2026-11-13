# Research

Use when the answer is built from many sources (analysis, comparison, literature review). A single fact stays a
plain search (`SKILL.md`).

## Steps

1. Ask only when the answer would change the research: a term with two meanings, an assumed place, a name shared by
   two entities. Otherwise start.
2. Split into non-overlapping subtopics that together cover the question, carrying the user's limits (dates, places,
   exclusions) into each. Use 3 when unsure, 4 for a comparison with separate criteria, 5 for separate entities,
   regions or domains, more only for a large set.
3. Research every subtopic (see Researcher). Subagents are optional: give each the subtopic, key questions, the
   user's limits and a notes-file path, and run them in one turn. Alone, keep findings with URLs in context or one
   scratchpad notes file.
4. Before writing, run another round for any part that would leave its section thin.
5. Write once (see Report); save to a file only if asked, then give the path. If a report on the subject already
   exists in this conversation, research only what is new or needs rechecking.

## Researcher

Cover only your subtopic. The notes are the only material the report uses, so every usable finding needs its URL.

- Name the gaps against the key questions, search them, then extract the pages you expect to cite; a number, date or
  quote needs the page, not a snippet.
- Prefer the authoritative source (company site, paper, filing, dataset, official docs, regulator, standards body)
  and say which source is which. For a recent topic trust what you retrieved over training data.
- About eight searches and extracts per subtopic; stop by twelve, or when the key questions have sourced answers or
  searches return pages you already have.
- Write a finding only if you can point at its page. Sources disagreeing: record both, authoritative first. No
  reliable source: record a gap. Leave out bare predictions, unnamed sources and aggregator-only figures unless
  labeled so.

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

Write from the notes only; do not search again. A claim with no URL in the notes does not go in. New notes win over
an earlier report, and the report stands on its own.

```markdown
# {Title: about six words, with a verb, stating the finding}

{One paragraph that answers the question, then the stakes and context. Someone who stops here knows the answer.}

## {Signpost heading that states a specific finding}
{Prose. **Bold** the figures and names a reader would scan for. Cite checkable claims as ([Source](URL)).}

## Conclusion
{One or two paragraphs on what the findings imply. Nothing restated from above.}
```

- One section per part of the question; each named entity, region or mechanism gets a subsection with its figures.
- Sentence-case headings that state the result ("Usage pricing costs more past 50 seats", not "Pricing").
- Table when items share fields. Cite figures, dates and attributed positions inline.
- Sources disagree: say so, name the one you take and why, state what nobody confirmed. Take the position the
  evidence supports instead of padding with "may".
