# Core — Read, Write, Convert, Parse & Compute Statistics on MSAs

> Source: former `bio-alignment-core` skill (primary tool: `Bio.AlignIO` / `Bio.Align`).

## Version Compatibility

Reference examples tested with: BioPython 1.83+, numpy 1.26+

Before using code patterns, verify installed versions match. If versions differ:
- Python: `pip show <package>` then `help(module.function)` to check signatures

If code throws ImportError, AttributeError, or TypeError, introspect the installed
package and adapt the example to match the actual API rather than retrying.

# Alignment Core

End-to-end multiple sequence alignment (MSA) handling: read/write/convert files,
parse and clean content, and calculate conservation, identity, entropy, and quality
metrics. One unified import block and one set of conventions cover all three.

## When to Use

| Goal | Section |
|------|---------|
| Read, write, or convert an alignment file (Clustal, PHYLIP, Stockholm, FASTA, NEXUS, MAF) | File I/O |
| Pick a format for a downstream tool, or preserve annotations | File I/O |
| Find conserved positions, build a consensus, analyze gaps, trim, filter, or map coordinates | Parsing & Extraction |
| Calculate percent identity, conservation, Shannon entropy, information content, PSSM, or sum-of-pairs | Statistics & Metrics |
| Judge whether an alignment is reliable before phylogenetics or selection analysis | Quality Assessment & Best Practices |

For generating alignments, see `msa-generation.md` (MAFFT/MUSCLE5/ClustalOmega) or
`pairwise.md` (PairwiseAligner). This reference assumes an alignment already exists.

## Quick Import Reference

One import block covers file I/O, parsing, and statistics. Import only what a task needs.

```python
from Bio import AlignIO                            # read/write/convert (MultipleSeqAlignment)
from Bio import Align                              # modern Alignment objects (counts, substitutions)
from Bio.Align import MultipleSeqAlignment         # build alignments programmatically
from Bio.Align import substitution_matrices        # BLOSUM62 etc. for sum-of-pairs
from Bio.SeqRecord import SeqRecord
from Bio.Seq import Seq
from collections import Counter                    # column composition
import numpy as np                                 # identity matrices
import math                                         # entropy
```

**Column-iteration pattern (used everywhere below).** Every per-column metric in this
skill walks the alignment the same way — slice the column string, tally with `Counter`,
and (usually) drop gaps. This pattern is stated once here and reused throughout:

```python
for col_idx in range(alignment.get_alignment_length()):
    column = alignment[:, col_idx]          # column as a string, e.g. 'AAAGA'
    counts = Counter(column.replace('-', '') or column)  # drop gaps when meaningful
    # ... derive conservation / consensus / entropy / composition from counts ...
```

Position numbering is 0-based throughout (first column is index 0).

## File I/O

`AlignIO.read()` for single-alignment files, `AlignIO.parse()` for multi-alignment files
(more memory efficient on large inputs), `AlignIO.write()` to save, `AlignIO.convert()`
for one-step format conversion. Indexing and slicing work on the alignment object:
`alignment[i]` (a sequence), `alignment[:, start:end]` (column range), `alignment[:, j]`
(one column as a string), `alignment[0:5, 50:150]` (both).

```python
alignment = AlignIO.read('alignment.aln', 'clustal')
AlignIO.write(alignment, 'output.fasta', 'fasta')
AlignIO.convert('input.aln', 'clustal', 'output.phy', 'phylip-relaxed')
```

See the Usage Guide section below for multi-alignment parsing, writing to handles,
alphabet-specified conversion, programmatic construction with `MultipleSeqAlignment`,
and batch directory conversion.

### Supported Formats

| Format | Extension | Read | Write | Description |
|--------|-----------|------|-------|-------------|
| `clustal` | .aln | Yes | Yes | Clustal W/X output |
| `fasta` | .fasta, .fa | Yes | Yes | Aligned FASTA |
| `phylip` | .phy | Yes | Yes | Interleaved PHYLIP |
| `phylip-sequential` | .phy | Yes | Yes | Sequential PHYLIP |
| `phylip-relaxed` | .phy | Yes | Yes | PHYLIP with long names |
| `stockholm` | .sto, .stk | Yes | Yes | Pfam/Rfam annotated |
| `nexus` | .nex | Yes | Yes | NEXUS format |
| `emboss` | .txt | Yes | No | EMBOSS tools output |
| `fasta-m10` | .txt | Yes | No | FASTA -m 10 output |
| `maf` | .maf | Yes | Yes | Multiple Alignment Format |
| `mauve` | .xmfa | Yes | No | progressiveMauve output |
| `msf` | .msf | Yes | No | GCG MSF format |

### Format Selection for Downstream Tools

| Downstream Tool | Required Format | BioPython Format String |
|----------------|-----------------|------------------------|
| RAxML-NG, IQ-TREE | PHYLIP (relaxed) | `'phylip-relaxed'` |
| MrBayes | NEXUS | `'nexus'` |
| PAUP* | NEXUS or PHYLIP | `'nexus'` or `'phylip'` |
| HMMER, Infernal | Stockholm | `'stockholm'` |
| Pfam/Rfam databases | Stockholm | `'stockholm'` |
| PAML/codeml | PHYLIP (sequential) | `'phylip-sequential'` |
| Most tools | FASTA | `'fasta'` |

### PHYLIP Format Pitfalls

PHYLIP has two incompatible layout variants (interleaved vs sequential) and two
name-length modes (strict vs relaxed). Confusing these causes silent data corruption.
**Strict PHYLIP** truncates names to exactly 10 characters, which can silently merge
distinct sequences sharing a 10-char prefix (`Homo_sapiens_chr1` and `Homo_sapiens_chr2`
both become `Homo_sapie`). Always prefer `phylip-relaxed` for writing unless a tool
specifically requires strict format; use `phylip-sequential` for PAML/codeml.

### Stockholm Annotations

Stockholm (Pfam, Rfam, HMMER) supports four annotation line types:

| Line Prefix | Scope | Description | Example |
|-------------|-------|-------------|---------|
| `#=GF` | File | Alignment-level metadata (ID, accession, description) | `#=GF AC PF00001` |
| `#=GC` | Column | Per-column annotation (1 char per column) | `#=GC SS_cons ..(((...)))..` |
| `#=GS` | Sequence | Per-sequence free text (organism, description) | `#=GS seq1 OS Homo sapiens` |
| `#=GR` | Residue | Per-residue annotation (1 char per residue) | `#=GR seq1 SS ..HHH..EEE..` |

Common GC annotations: `SS_cons` (consensus secondary structure), `RF` (reference
coordinates), `seq_cons`. Rfam RNA families use `<>` for base pairs, `.` for unpaired.
Per-record annotations are reachable via `record.annotations` and
`record.letter_annotations['secondary_structure']`; column annotations via
`alignment.column_annotations`.

### Annotation Preservation

Not all formats carry annotations. Converting can silently discard metadata:

| Format | Sequence Annotations | Column Annotations | Secondary Structure |
|--------|---------------------|-------------------|-------------------|
| Stockholm | Yes (GS/GR lines) | Yes (GC lines) | Yes (SS_cons) |
| NEXUS | Partial (SETS block) | Via CHARSET | No |
| Clustal | No (conservation marks not parsed) | No | No |
| PHYLIP | No | No | No |
| FASTA | No | No | No |

Stockholm-to-FASTA/PHYLIP discards all annotations, secondary structure, and per-residue
quality scores. If annotations matter, keep a Stockholm master copy.

### Bio.AlignIO vs Bio.Align

The newer `Bio.Align` module provides `Align.read()`, `Align.parse()`, `Align.write()`
returning `Alignment` objects (vs `MultipleSeqAlignment`), with modern features like
`.substitutions`.

| Use Case | Module |
|----------|--------|
| Legacy code, MultipleSeqAlignment needed | `Bio.AlignIO` |
| Modern features (counts, substitutions) | `Bio.Align` |
| Format conversion | Either works |
| Working with pairwise alignments | `Bio.Align` |

## Parsing & Extraction

Extract sequence info (`[r.id for r in alignment]`, `[str(r.seq) for r in alignment]`,
lookup by ID), analyze columns, handle gaps, derive a consensus, filter sequences, and
map coordinates. All column work uses the Counter pattern above.

- **Conserved positions** — most-common non-gap residue fraction ≥ threshold per column.
- **Gap analysis** — gaps per sequence, gaps per column, find/remove columns above a gap
  fraction (`remove_gappy_columns` rebuilds a `MultipleSeqAlignment` from kept columns).
- **Consensus** — majority-rule per column with an ambiguity fallback below threshold.
- **Region extraction** — slice column ranges, or project all sequences onto a reference's
  ungapped columns.
- **Filtering** — by ID regex, by per-sequence gap content, or de-duplication.
- **Position mapping** — convert alignment column ↔ ungapped sequence coordinate by
  walking the sequence and tracking gaps.

Full implementations of `find_conserved_positions`, `gaps_per_column`,
`find_gappy_columns`, `remove_gappy_columns`, `consensus_sequence`,
`extract_ungapped_regions`, `filter_by_id`, `filter_by_gap_content`,
`remove_duplicates`, and both position-mapping functions are in the Usage Guide below.

> `Bio.Align.AlignInfo.SummaryInfo` is **deprecated** in recent Biopython. Use the custom
> `consensus_sequence()` / `position_specific_score_matrix()` functions in the Usage Guide
> instead of `AlignInfo`.

### Trimming: Decision Framework

Trimming (removing unreliable columns before downstream analysis) is **controversial** —
research is split on whether it helps or hurts phylogenetic inference. The approach
matters more than whether to trim.

| Tool | Approach | When to Use |
|------|----------|-------------|
| ClipKIT | Retains parsimony-informative + constant sites | Default choice; `kpic-smart-gap` mode consistently outperforms others |
| trimAl `-automated1` | Gap + similarity scoring | When ClipKIT unavailable; equal or better than unfiltered in most tests |
| Gblocks (relaxed params) | Block-based removal | Only with relaxed parameters; defaults are too aggressive |

**Key insight**: ClipKIT inverts the traditional approach — instead of removing "bad"
columns it retains informative ones, which consistently produces better trees.
**When NOT to trim**: single-gene phylogenetics with well-curated alignments. Aggressive
trimming (>20-30% of sites removed) causes rapid tree deterioration.

```bash
clipkit alignment.fasta -m kpic-smart-gap -o trimmed.fasta   # recommended
trimal -in alignment.fasta -out trimmed.fasta -automated1
trimal -in alignment.fasta -out trimmed.fasta -gt 0.5        # explicit gap threshold
```

### Gap Handling for Phylogenetics

How gaps are treated downstream significantly affects tree topology:

| Treatment | Method | Tradeoff |
|-----------|--------|----------|
| Missing data (default) | Gaps = unknown character | Most common; can be statistically inconsistent under ML |
| Fifth state | Gap = 5th nucleotide | Biologically problematic (different-length gaps treated equally) |
| Simple indel coding | Each unique indel coded as binary character | Most biologically realistic; adds phylogenetic signal |

Indel coding and fifth-state outperform missing-data treatment ~90% of the time on
empirical datasets. For important analyses, consider indel coding.

## Statistics & Metrics

### Percent Identity Definitions

Four common denominators produce **up to 11.5% difference** on the same alignment;
combined with different alignment algorithms, variation reaches 22%. Always report which
method was used.

| Method | Denominator | Notes |
|--------|-------------|-------|
| PID1 | Aligned positions including internal gaps | `a != '-' or b != '-'` |
| PID2 | Aligned residue pairs only (no gaps) | Always the highest value |
| PID3 | Shorter ungapped sequence length | — |
| PID4 | Mean ungapped sequence length | Best correlation with structural similarity (r=0.86); recommended for evolutionary analyses |

A single `pairwise_identity(seq1, seq2, method=...)` function and an N×N
`identity_matrix()` are in the Usage Guide below.

### Conservation, Entropy, Information Content

- **Conservation** — per-column most-common-residue fraction (ignore gaps);
  `average_conservation` and a sliding-window `conservation_profile` aggregate it.
- **Shannon entropy** — `-Σ p·log2(p)` over column character frequencies; lower = more
  conserved. Range 0 to log2(alphabet).
- **Information content** — `log2(alphabet_size) - entropy` (alphabet 4 for DNA, 20 for
  protein). Negative IC means the wrong alphabet size was used.

Conservation and entropy are inversely related; compute both ignoring gaps for
interpretable results.

### Substitutions, PSSM, Scoring

- **Substitution counts** — tally all pairwise non-gap mismatches per column; or build a
  full per-character matrix. For pairwise alignments, `PairwiseAligner` output exposes a
  built-in `.substitutions` Array.
- **PSSM** — per-position dict of non-gap character counts for motif analysis/scoring.
- **Alignment score** — sum-of-pairs over columns with match/mismatch/gap weights, or
  `sum_of_pairs()` with a `substitution_matrices.load('BLOSUM62')` matrix (proteins;
  simple match/mismatch for DNA).

Implementations of `column_conservation`, `average_conservation`,
`conservation_profile`, `shannon_entropy`, `information_content`, `substitution_counts`,
`build_substitution_matrix`, `gap_profile`, `gap_statistics`, `alignment_score`,
`sum_of_pairs`, and `position_specific_score_matrix` are in the Usage Guide below.

### Metric Reference

| Metric | Description | Range |
|--------|-------------|-------|
| Identity | Fraction of identical residues | 0-1 |
| Conservation | Most common residue frequency | 0-1 |
| Shannon Entropy | Variability measure | 0 to log2(alphabet) |
| Information Content | Max entropy - observed entropy | 0 to log2(alphabet) |
| Gap Fraction | Proportion of gaps | 0-1 |

## Quality Assessment & Best Practices

### When to Worry About Alignment Quality

| Warning Sign | Implication | Action |
|-------------|-------------|--------|
| Average pairwise identity <25% (protein) | Twilight zone; alignment may be unreliable | Use GUIDANCE2 to assess; consider structural alignment |
| >30% of columns have >50% gaps | Possible non-homologous sequences or misalignment | Remove outlier sequences and re-align |
| Identity varies dramatically across regions | Domain architecture mismatch | Align domains separately |
| Conservation absent in expected functional regions | Alignment error or non-homology | Verify with BLAST that sequences are truly homologous |

The **twilight zone** (average pairwise protein identity <25%) is the single most
important reliability red flag: below it, alignment is questionable and structural
methods should be considered. Columns with **both** high gap fraction AND low conservation
are the strongest indicators of local alignment uncertainty, often reflecting guide-tree
artifacts rather than true evolution.

### Quantifying Uncertainty

For critical downstream analyses (phylogenetics, selection), quantify uncertainty rather
than assume it:

- **GUIDANCE2** — ~400 perturbed alignments varying guide trees, gap penalties, and
  co-optimal solutions. Column confidence = frequency across perturbations. Default
  reliability threshold: 0.93.
- **MUSCLE5 ensemble** — alignments with perturbed HMM parameters; column confidence =
  fraction of ensemble members supporting that column. Faster than GUIDANCE2.

Alignment uncertainty propagates directly into evolutionary inference — different aligners
can support fundamentally different phylogenies, and for dN/dS, misaligned codons create
false-positive selection signals. Always report the alignment method and consider a
sensitivity analysis.

## Common Errors

| Error | Cause | Solution |
|-------|-------|----------|
| `ValueError: No records` | Empty file | Check file path and format |
| `ValueError: More than one record` | Multiple alignments with `read()` | Use `parse()` instead |
| `ValueError: Sequences different lengths` | Invalid alignment | Ensure all sequences same length |
| `ValueError: unknown format` | Unsupported format string | Check supported formats list |
| `IndexError` | Column index out of range | Check `get_alignment_length()` |
| Empty Counter | All gaps in column | Handle gap-only columns |
| `ZeroDivisionError` | Empty column after gap removal | Check for gap-only columns |
| `KeyError` | Character not in substitution matrix | Handle gaps separately |
| Negative IC | Wrong alphabet size | Use 4 for DNA, 20 for protein |

---

# Usage Guide (deep code examples)

## Overview

This skill provides end-to-end handling of multiple sequence alignments (MSAs) with
Biopython: reading, writing, and converting alignment files; parsing and cleaning their
content; and calculating statistics. It merges file I/O (`Bio.AlignIO`), content parsing,
and statistical metrics into one interface.

## Prerequisites

```bash
pip install biopython numpy
```

## Quick Start

Tell your AI agent what you want to do:
- "Read this Clustal alignment file and show me the sequences"
- "Convert my PHYLIP alignment to FASTA format"
- "Find all the fully conserved positions in this alignment"
- "Remove sequences with more than 10% gaps, then re-save"
- "Calculate a pairwise identity matrix and the conservation profile"

## Example Prompts

### File I/O
> "Load the alignment from alignment.aln and tell me how many sequences it has"

> "Parse all alignments from this Stockholm file"

> "Convert my Clustal alignment to PHYLIP-relaxed format"

> "Convert all .aln files in this directory to FASTA"

> "Write the alignment to Nexus format for MrBayes"

### Parsing & Cleaning
> "Show me the composition of each column in the alignment"

> "Find positions that are conserved in at least 80% of sequences"

> "Remove columns with more than 50% gaps"

> "Generate a consensus sequence with 70% threshold"

> "Remove duplicate sequences from the alignment"

> "Map alignment column 120 to the ungapped position in species_A"

### Statistics
> "Create a pairwise identity matrix for this alignment"

> "Calculate Shannon entropy for each column"

> "What is the information content at each position?"

> "Build a substitution matrix from this alignment"

> "Score the alignment with sum-of-pairs using BLOSUM62"

## What the Agent Will Do

1. Load the alignment file with the appropriate format parser
2. Access sequences, IDs, columns, and annotations
3. Perform requested parsing, filtering, or metric calculation
4. Summarize results (statistics, matrices, profiles, filtered alignment)
5. Optionally write output in the specified format

## Key Concepts

| Term | Description |
|------|-------------|
| Column | Vertical slice (same position across all sequences) |
| Conservation | Fraction of sequences with same residue at a position |
| Consensus | Most common character at each position |
| Gap | Missing data represented by '-' |
| Identity | Fraction of identical aligned residues between two sequences |
| Entropy | Column variability (lower = more conserved) |

---

## File I/O

### Reading

```python
from Bio import AlignIO

# Single alignment
alignment = AlignIO.read('alignment.aln', 'clustal')
print(f'Alignment length: {alignment.get_alignment_length()}')
print(f'Number of sequences: {len(alignment)}')

# Multiple alignments in one file
for alignment in AlignIO.parse('multi_alignment.sto', 'stockholm'):
    print(f'{len(alignment)} sequences, length {alignment.get_alignment_length()}')

# Read all as a list (parse() is more memory efficient for large files)
alignments = list(AlignIO.parse('alignments.phy', 'phylip'))
```

### Writing

```python
AlignIO.write(alignment, 'output.fasta', 'fasta')                 # single
count = AlignIO.write([a1, a2, a3], 'output.sto', 'stockholm')    # multiple

with open('output.aln', 'w') as handle:                           # to a handle
    AlignIO.write(alignment, handle, 'clustal')
```

### Conversion

```python
# Direct one-step (most efficient)
AlignIO.convert('input.aln', 'clustal', 'output.phy', 'phylip-relaxed')

# With alphabet specification
AlignIO.convert('input.sto', 'stockholm', 'output.nex', 'nexus', molecule_type='DNA')

# Manual when modification is needed
alignment = AlignIO.read('input.aln', 'clustal')
# ... modify ...
AlignIO.write(alignment, 'output.fasta', 'fasta')
```

### Accessing & Slicing

```python
for record in alignment:
    print(f'{record.id}: {record.seq}')

first_seq = alignment[0]
column_slice = alignment[:, 10:20]   # columns 10-19
column = alignment[:, 5]             # column 5 as a string
region = alignment[0:5, 50:150]      # 5 sequences, columns 50-149
seq_ids = [record.id for record in alignment]
```

### Programmatic Construction

```python
from Bio.Align import MultipleSeqAlignment
from Bio.SeqRecord import SeqRecord
from Bio.Seq import Seq

records = [
    SeqRecord(Seq('ACTGACTGACTG'), id='seq1'),
    SeqRecord(Seq('ACTGACT-ACTG'), id='seq2'),
    SeqRecord(Seq('ACTG-CTGACTG'), id='seq3'),
]
alignment = MultipleSeqAlignment(records)
AlignIO.write(alignment, 'new_alignment.fasta', 'fasta')
```

### Stockholm Annotations

```python
alignment = AlignIO.read('pfam.sto', 'stockholm')
for record in alignment:
    print(record.id, record.annotations)
    if 'secondary_structure' in record.letter_annotations:
        print(f'  SS: {record.letter_annotations["secondary_structure"]}')
# Column annotations: alignment.column_annotations
```

### Batch Directory Conversion

```python
from pathlib import Path

input_dir = Path('alignments/')
output_dir = Path('converted/')
for input_file in input_dir.glob('*.aln'):
    alignment = AlignIO.read(input_file, 'clustal')
    AlignIO.write(alignment, output_dir / f'{input_file.stem}.fasta', 'fasta')
```

### Modern Bio.Align I/O

```python
from Bio import Align

alignment = Align.read('alignment.aln', 'clustal')   # returns Alignment object
for alignment in Align.parse('multi.sto', 'stockholm'):
    print(f'{len(alignment)} sequences')
Align.write(alignment, 'output.fasta', 'fasta')
```

---

## Parsing & Extraction

### Sequence Information

```python
seq_ids = [record.id for record in alignment]
sequences = [str(record.seq) for record in alignment]

def get_sequence_by_id(alignment, seq_id):
    for record in alignment:
        if record.id == seq_id:
            return record
    return None
```

### Column Composition & Conserved Positions

```python
from collections import Counter

def column_composition(alignment, col_idx):
    return Counter(alignment[:, col_idx])

def find_conserved_positions(alignment, threshold=1.0):
    conserved = []
    for col_idx in range(alignment.get_alignment_length()):
        column = alignment[:, col_idx]
        counts = Counter(column)
        most_common_char, most_common_count = counts.most_common(1)[0]
        if most_common_char != '-':
            if most_common_count / len(alignment) >= threshold:
                conserved.append((col_idx, most_common_char))
    return conserved

fully_conserved = find_conserved_positions(alignment, threshold=1.0)
mostly_conserved = find_conserved_positions(alignment, threshold=0.8)
```

### Gap Analysis

```python
gap_counts = [(record.id, str(record.seq).count('-')) for record in alignment]

def gaps_per_column(alignment):
    return [alignment[:, i].count('-') for i in range(alignment.get_alignment_length())]

def find_gappy_columns(alignment, threshold=0.5):
    n = len(alignment)
    return [i for i in range(alignment.get_alignment_length())
            if alignment[:, i].count('-') / n >= threshold]

def remove_gappy_columns(alignment, threshold=0.5):
    n = len(alignment)
    keep = [i for i in range(alignment.get_alignment_length())
            if alignment[:, i].count('-') / n < threshold]
    new_records = []
    for record in alignment:
        new_seq = ''.join(str(record.seq)[i] for i in keep)
        new_records.append(SeqRecord(Seq(new_seq), id=record.id, description=record.description))
    return MultipleSeqAlignment(new_records)

cleaned = remove_gappy_columns(alignment, threshold=0.5)
```

### Consensus

`Bio.Align.AlignInfo.SummaryInfo` is **deprecated**; use this custom function instead.

```python
def consensus_sequence(alignment, threshold=0.5, gap_char='-', ambiguous='N'):
    consensus = []
    for col_idx in range(alignment.get_alignment_length()):
        column = alignment[:, col_idx]
        counts = Counter(column)
        most_common_char, most_common_count = counts.most_common(1)[0]
        if most_common_char == gap_char:
            counts.pop(gap_char, None)
            if counts:
                most_common_char, most_common_count = counts.most_common(1)[0]
            else:
                most_common_char = gap_char
        if most_common_count / len(alignment) >= threshold:
            consensus.append(most_common_char)
        else:
            consensus.append(ambiguous)
    return ''.join(consensus)
```

### Region Extraction

```python
region = alignment[:, 100:200]   # column range
subset = alignment[0:10]         # sequence range

def extract_ungapped_regions(alignment, ref_idx=0):
    ref_seq = str(alignment[ref_idx].seq)
    ungapped_cols = [i for i, char in enumerate(ref_seq) if char != '-']
    new_records = []
    for record in alignment:
        new_seq = ''.join(str(record.seq)[i] for i in ungapped_cols)
        new_records.append(SeqRecord(Seq(new_seq), id=record.id, description=record.description))
    return MultipleSeqAlignment(new_records)
```

### Sequence Filtering

```python
import re

def filter_by_id(alignment, pattern):
    regex = re.compile(pattern)
    return MultipleSeqAlignment([r for r in alignment if regex.search(r.id)])

def filter_by_gap_content(alignment, max_gap_fraction=0.1):
    filtered = [r for r in alignment
                if str(r.seq).count('-') / len(r.seq) <= max_gap_fraction]
    return MultipleSeqAlignment(filtered)

def remove_duplicates(alignment):
    seen, unique = set(), []
    for record in alignment:
        seq_str = str(record.seq)
        if seq_str not in seen:
            seen.add(seq_str)
            unique.append(record)
    return MultipleSeqAlignment(unique)
```

### Position Mapping

```python
def alignment_to_sequence_position(record, align_pos):
    seq_pos = 0
    for i, char in enumerate(str(record.seq)):
        if i == align_pos:
            return seq_pos if char != '-' else None
        if char != '-':
            seq_pos += 1
    return None

def sequence_to_alignment_position(record, seq_pos):
    current = 0
    for i, char in enumerate(str(record.seq)):
        if char != '-':
            if current == seq_pos:
                return i
            current += 1
    return None
```

---

## Statistics & Metrics

### Pairwise Identity (4 methods)

```python
def pairwise_identity(seq1, seq2, method='pid1'):
    matches = sum(a == b and a != '-' for a, b in zip(seq1, seq2))
    if method == 'pid1':
        denom = sum(a != '-' or b != '-' for a, b in zip(seq1, seq2))
    elif method == 'pid2':
        denom = sum(a != '-' and b != '-' for a, b in zip(seq1, seq2))
    elif method == 'pid3':
        denom = min(len(seq1.replace('-', '')), len(seq2.replace('-', '')))
    elif method == 'pid4':
        denom = (len(seq1.replace('-', '')) + len(seq2.replace('-', ''))) / 2
    return matches / denom if denom > 0 else 0

seq1, seq2 = str(alignment[0].seq), str(alignment[1].seq)
for method in ['pid1', 'pid2', 'pid3', 'pid4']:
    print(f'{method}: {pairwise_identity(seq1, seq2, method) * 100:.1f}%')
```

```python
import numpy as np

def identity_matrix(alignment):
    n = len(alignment)
    matrix = np.zeros((n, n))
    for i in range(n):
        for j in range(i, n):
            ident = pairwise_identity(str(alignment[i].seq), str(alignment[j].seq))
            matrix[i, j] = matrix[j, i] = ident
    return matrix
```

### Conservation

```python
def column_conservation(alignment, col_idx, ignore_gaps=True):
    column = alignment[:, col_idx]
    if ignore_gaps:
        column = column.replace('-', '')
    if not column:
        return 0.0
    return Counter(column).most_common(1)[0][1] / len(column)

def average_conservation(alignment, ignore_gaps=True):
    scores = [column_conservation(alignment, i, ignore_gaps)
              for i in range(alignment.get_alignment_length())]
    return sum(scores) / len(scores)

def conservation_profile(alignment, window=10):
    L = alignment.get_alignment_length()
    profile = []
    for i in range(L):
        start, end = max(0, i - window // 2), min(L, i + window // 2)
        scores = [column_conservation(alignment, j) for j in range(start, end)]
        profile.append(sum(scores) / len(scores))
    return profile
```

### Shannon Entropy & Information Content

```python
import math

def shannon_entropy(column, ignore_gaps=True):
    if ignore_gaps:
        column = column.replace('-', '')
    if not column:
        return 0.0
    total = len(column)
    entropy = 0.0
    for count in Counter(column).values():
        p = count / total
        if p > 0:
            entropy -= p * math.log2(p)
    return entropy

def information_content(column, alphabet_size=4):   # 4 for DNA, 20 for protein
    return math.log2(alphabet_size) - shannon_entropy(column)
```

### Substitution Counts & Matrix

```python
from collections import defaultdict

def substitution_counts(alignment):
    counts = defaultdict(int)
    for col_idx in range(alignment.get_alignment_length()):
        chars = [c for c in alignment[:, col_idx] if c != '-']
        for i, c1 in enumerate(chars):
            for c2 in chars[i + 1:]:
                if c1 != c2:
                    counts[tuple(sorted([c1, c2]))] += 1
    return dict(counts)

def build_substitution_matrix(alignment):
    matrix = defaultdict(lambda: defaultdict(int))
    for col_idx in range(alignment.get_alignment_length()):
        chars = [c for c in alignment[:, col_idx] if c != '-']
        for c1 in chars:
            for c2 in chars:
                matrix[c1][c2] += 1
    return {k: dict(v) for k, v in matrix.items()}
```

For pairwise alignments, use the built-in `.substitutions` property:

```python
from Bio.Align import PairwiseAligner

aligner = PairwiseAligner(mode='global', match_score=1, mismatch_score=-1)
alignments = aligner.align(seq1, seq2)
print(alignments[0].substitutions)
```

### Gap Statistics

```python
def gap_profile(alignment):
    n = len(alignment)
    return [alignment[:, i].count('-') / n
            for i in range(alignment.get_alignment_length())]

def gap_statistics(alignment):
    num_seqs = len(alignment)
    num_cols = alignment.get_alignment_length()
    total_gaps = sum(str(r.seq).count('-') for r in alignment)
    gaps_per_seq = [str(r.seq).count('-') for r in alignment]
    gaps_per_col = [alignment[:, i].count('-') for i in range(num_cols)]
    return {
        'total_gaps': total_gaps,
        'gap_fraction': total_gaps / (num_seqs * num_cols),
        'gappiest_seq': max(range(num_seqs), key=lambda i: gaps_per_seq[i]),
        'gappiest_col': max(range(num_cols), key=lambda i: gaps_per_col[i]),
        'gap_free_cols': sum(1 for g in gaps_per_col if g == 0),
    }
```

### Alignment Scoring

```python
def alignment_score(alignment, match=1, mismatch=-1, gap=-2):
    total = 0
    for col_idx in range(alignment.get_alignment_length()):
        column = alignment[:, col_idx]
        for i, c1 in enumerate(column):
            for c2 in column[i + 1:]:
                if c1 == '-' or c2 == '-':
                    total += gap
                elif c1 == c2:
                    total += match
                else:
                    total += mismatch
    return total

def sum_of_pairs(alignment, substitution_matrix=None):
    from Bio.Align import substitution_matrices
    if substitution_matrix is None:
        substitution_matrix = substitution_matrices.load('BLOSUM62')
    total = 0
    for col_idx in range(alignment.get_alignment_length()):
        column = alignment[:, col_idx]
        for i, c1 in enumerate(column):
            for c2 in column[i + 1:]:
                if c1 != '-' and c2 != '-':
                    total += substitution_matrix.get((c1, c2), 0)
    return total
```

### PSSM

```python
def position_specific_score_matrix(alignment):
    pssm = []
    for col_idx in range(alignment.get_alignment_length()):
        counts = Counter(alignment[:, col_idx])
        counts.pop('-', None)
        pssm.append(dict(counts))
    return pssm
```

---

## Tips

- **Always use `phylip-relaxed` over `phylip`** unless a downstream tool requires strict
  format. Strict PHYLIP truncates names to 10 characters and can silently merge distinct
  sequences sharing a prefix.
- Stockholm preserves annotations (secondary structure, per-residue quality, metadata)
  that all other formats lose. Keep a Stockholm master copy if annotations matter.
- For large alignments, `parse()` is more memory efficient than `read()`.
- Position numbering is 0-based (first column is index 0).
- Always check gap content before phylogenetic analysis. Columns with >50% gaps often
  indicate alignment artifacts or non-homologous sequences, and can reflect guide-tree
  topology rather than true evolution.
- For trimming before phylogenetics, prefer ClipKIT (`kpic-smart-gap` mode); aggressive
  trimming (>20-30% of sites) can hurt tree quality.
- Conservation thresholds depend on alignment diversity — 80% conservation in a
  5-sequence alignment means less than in a 500-sequence alignment.
- Percent identity has **four definitions** producing up to 11.5% difference; always
  specify which (PID4 = mean length, recommended for evolutionary studies).
- Conservation and entropy are inversely related; compute both ignoring gaps.
- For proteins, score with BLOSUM62; for DNA, use simple match/mismatch.
- Average pairwise identity <25% (protein) signals the twilight zone where alignment
  reliability is questionable and structural methods should be considered.
- For critical analyses, quantify per-column confidence with GUIDANCE2 (threshold 0.93)
  or a MUSCLE5 ensemble before inference, and report the alignment method used.
