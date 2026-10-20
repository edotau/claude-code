# Live-first examples

The failure mode this prevents is subtle because it looks like diligence: a test suite grows
around code that has never processed an actual input. Every test passes. Nothing has been
learned. Tests written before first execution encode **what you assumed the data looks
like**, and when the assumption is wrong they still pass, which is worse than failing —
a red test tells you something, a green test built on a wrong premise tells you nothing
while feeling like progress.

This is not an argument against test-first. Test-first is right when the contract is
**already known**: a specified API, a documented format, a defect whose shape you have
already reproduced. It is wrong when the contract is being **discovered from the data** —
a vendor export's column names, which sign means destabilising, what an endpoint actually
returns, whether a file's numbering matches the sequence you think it does. There the data
is the specification, and you cannot write a meaningful assertion before reading it.

### The tell

Ask: *has this code ever consumed a real input?* If the answer is no and the test count is
climbing, stop and go get an input. Two specific smells:

- the only fixtures are **synthetic** ones you constructed from your own beliefs about the
  format
- a supporting document (interpretation memo, README, integration guide) already states
  conclusions that no execution has produced

### Worked example of getting it wrong

A residue-scan reader was built to parse a vendor export: the module, a parent-alignment
stage, a sign-convention verifier, a synthetic drifted fixture, passing tests, and an
interpretation memo describing what the numbers meant. It had **never been run on the real
export**, because the file was never supplied. Every number in that memo traced back to a
pasted narrative or to the fixture — that is, to assumptions.

Separately, the same effort produced three test files around a defect fix, none of which
imported the module under test (see the import rule below). Time went into test volume
while the two things that would have produced knowledge — running the reader on the real
CSV, and importing the function under test — went undone.

The import rule: [guard examples](guard-examples.md#the-import-rule).

### When the data is not available

Name exactly what is needed — for the reader above: the export plus the parent
sequences, since parent alignment cannot run without them — and mark anything already
written as unverified until it arrives. A suite built over an absent input is not progress
banked; it is a claim to unpick later.
