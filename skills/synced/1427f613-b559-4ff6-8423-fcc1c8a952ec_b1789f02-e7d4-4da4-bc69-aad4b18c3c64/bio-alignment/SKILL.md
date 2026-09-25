---
name: bio-alignment
description: Sequence alignment toolkit — read/write/convert MSA files (Clustal, PHYLIP, Stockholm, FASTA, NEXUS, MAF) and compute statistics (conservation, percent identity, gaps, consensus, Shannon entropy, PSSM, sum-of-pairs); pairwise alignment with Bio.Align.PairwiseAligner (global/local/semi-global, BLOSUM/PAM, affine gaps); multiple sequence alignment with MAFFT, MUSCLE5, ClustalOmega, T-Coffee (plus codon-aware PAL2NAL/MACSE); and homology search via remote NCBI BLAST, PSI-BLAST, HMMER/jackhmmer, reciprocal best hits (RBH), and Delta-BLAST. Trigger on align, alignment, MSA, BLAST, blastp, blastn, homology, homolog, ortholog, MAFFT, MUSCLE, ClustalOmega, HMMER, Pfam, consensus, conservation, percent identity, PairwiseAligner, Needleman-Wunsch, Smith-Waterman.
---
# Bio-Alignment

Unified skill for sequence alignment and homology search. It covers four task
families; each has a dedicated reference file under `reference/`. Read this router
first, then open the one reference file that matches the task — the references hold
the full code examples, version-compatibility notes, decision tables, and tips.

## Version Compatibility (at a glance)

Reference examples tested with: BioPython 1.83+, numpy 1.26+, MAFFT 7.520+,
MUSCLE 5.1+, ClustalOmega 1.2.4+, T-Coffee 13+, PAL2NAL 14+, NCBI BLAST+ 2.15+,
HMMER 3.3+. Each reference restates its own pinned versions. If a code pattern
throws ImportError/AttributeError/TypeError, introspect the installed package
(`pip show <pkg>`, `help(...)`) or CLI (`<tool> --version/--help`) and adapt rather
than retrying blindly.

## Task → Reference Router

| If the task is… | Open |
|-----------------|------|
| Read / write / convert an alignment file (Clustal, PHYLIP, Stockholm, FASTA, NEXUS, MAF); pick a format for a downstream tool; preserve annotations | `reference/core.md` |
| Compute on an **existing** alignment: conservation, percent identity, gaps, consensus, Shannon entropy, information content, PSSM, sum-of-pairs; trim/filter columns or sequences; map coordinates; assess alignment quality / twilight zone | `reference/core.md` |
| Align **two** sequences (global / local / semi-global, Needleman-Wunsch / Smith-Waterman); choose scoring (match/mismatch, BLOSUM/PAM, affine gaps); percent-identity definitions; alignment significance | `reference/pairwise.md` |
| Align **three or more** sequences: MAFFT (L-INS-i / FFT-NS-2 / E-INS-i / auto), MUSCLE5 (incl. ensemble confidence), ClustalOmega, T-Coffee; add sequences to an existing MSA; codon-aware alignment (PAL2NAL / MACSE) for dN/dS | `reference/msa-generation.md` |
| Find homologs / search a database: remote NCBI BLAST (blastn/blastp/blastx/tblastn), PSI-BLAST, HMMER / jackhmmer / hmmscan (Pfam), reciprocal best hits (orthologs), Delta-BLAST | `reference/homology-search.md` |

## Quick Orientation

```
Need to FIND related sequences?            → reference/homology-search.md  (BLAST / PSI-BLAST / HMMER / RBH)
Have sequences, need to ALIGN them?
    ├── exactly 2 sequences                → reference/pairwise.md         (PairwiseAligner)
    └── 3+ sequences                       → reference/msa-generation.md   (MAFFT / MUSCLE5 / ClustalOmega / T-Coffee)
Already HAVE an alignment (file or MSA)?   → reference/core.md             (I/O, convert, stats, quality)
```

A typical end-to-end flow chains them: **homology-search** finds candidate
homologs → **msa-generation** (3+) or **pairwise** (2) aligns them →
**core** reads/converts the result and computes conservation, identity, entropy,
and quality metrics before phylogenetics or selection analysis.

## Cross-Cutting Concepts

These recur across all four references; learn them once:

- **Twilight zone** — average pairwise protein identity <25% means alignment
  reliability is questionable; consider profile-profile (HHpred) or structural
  methods. Covered in `core.md`, `pairwise.md`, and `msa-generation.md`.
- **Percent identity has four definitions** (PID1–PID4) producing up to 11.5%
  difference on one alignment; always report which. See `core.md` / `pairwise.md`.
- **Affine gap penalties** (gap open ≫ gap extend) model real indel biology; the
  BLASTP default is open=-11, extend=-1. See `pairwise.md`.
- **Guide-tree dependency & uncertainty** — progressive MSA never removes an
  inserted gap; quantify per-column confidence with a MUSCLE5 ensemble or
  GUIDANCE2. See `msa-generation.md` and `core.md`.
- **E-value vs bit score** — significance measures for database searches; E<1e-5
  is a common homology threshold. See `homology-search.md` and `pairwise.md`.

## Related Skills

- `bio-local-blast` — local BLAST+ CLI: custom databases, fast unlimited offline searches (complements `reference/homology-search.md`).
- `bio-entrez-access` — fetch full records for hits; search NCBI by keyword.
- `phylogenetics` — build trees from processed alignments (MAFFT → IQ-TREE / FastTree).
- `biopython` — broader molecular-biology toolkit (sequence I/O, manipulation).
