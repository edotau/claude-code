# Immunogenicity Prediction (scoring + neoantigen impact + TCR recognition)

> Source skill: `bio-immunoinformatics-immunogenicity-prediction`
> tool_type: python · primary_tool: mhcflurry

## Version Compatibility

Reference examples tested with: MHCflurry 2.1+, pVACtools 4.1+, Ensembl VEP 111+,
ERGO-II, MiXCR 4.6+, numpy 1.26+, pandas 2.2+, scikit-learn 1.4+, scipy 1.12+

Before using code patterns, verify installed versions match. If versions differ:
- Python: `pip show <package>` then `help(module.function)` to check signatures
- CLI: `<tool> --version` then `<tool> --help` to confirm flags

If code throws ImportError, AttributeError, or TypeError, introspect the installed
package and adapt the example to match the actual API rather than retrying.

# Immunogenicity Prediction

A T-cell immunogenicity decision pipeline that chains three stages:

1. **Epitope immunogenicity scoring** — multi-factor models combining MHC binding,
   processing, expression, and sequence foreignness to rank candidate peptides.
2. **Mutation & neoantigen impact** — somatic-variant-derived neoepitopes via
   pVACtools, agretopicity (DAI) scoring, and quality metrics.
3. **TCR recognition prediction** — match candidate epitopes to T-cell receptors
   with ERGO-II and VDJdb database matching.

PepX expression annotation provides the expression factor feeding stage 1.

> Deep, runnable code for every section lives in the **Usage Guide** section below.
> The first part is the navigable overview — read it to pick a stage, then open the
> guide for the implementation.

## When to use

- **"Rank my neoantigen candidates by immunogenicity"** → multi-factor scoring,
  tiering, and vaccine-candidate selection (stage 1).
- **"Identify neoantigens from my tumor mutations"** → pVACseq on a VEP-annotated
  VCF with patient HLA alleles, agretopicity, prioritization (stage 2).
- **"Predict which epitopes my TCRs recognize"** → ERGO-II / VDJdb matching and
  repertoire specificity analysis (stage 3).
- **"Which peptides are self-like / will be presented / are clonal?"** →
  foreignness, processing, expression, and clonality factors.

Skip this stage for raw binding-affinity prediction alone — use the
**mhc-binding-prediction** reference. This stage consumes binding
predictions as one input factor among several.

## T-cell immunogenicity scoring

Rank epitopes by a composite score combining weighted factors, then tier them for
prioritization. No single factor determines immunogenicity.

**Multi-factor model.** Each factor is scored 0-1 and combined via a weighted sum
with domain-informed weights:

| Factor | Meaning | Default weight |
|--------|---------|----------------|
| Binding | MHC binding affinity (lower IC50 = better) | 0.25 |
| Agretopicity | MT vs WT binding ratio (neoantigens) | 0.20 |
| Processing | Proteasomal cleavage + TAP transport | 0.10 |
| Expression | Source-protein expression (log TPM) | 0.15 |
| Clonality | VAF for neoantigens | 0.15 |
| Foreignness | 1 − self-similarity (tolerance avoidance) | 0.15 |

See `calculate_immunogenicity_score()` in the Usage Guide for the reference
implementation, including the 0-1 transforms for each factor.

**Processing prediction.** MHCflurry's `Class1ProcessingPredictor` scores the
probability that a peptide is cleaved from its source protein, transported by TAP,
and loaded onto MHC. Higher processing score = more likely presented. Surrounding
sequence context is needed in practice — extract it from the protein.

**Self-similarity (foreignness).** Compute pairwise identity of a candidate against
a proteome peptide set. High similarity to self-peptides (≥0.8 identity) suggests
T-cells may be tolerized (deleted during development) → lower likelihood of
response. The reference `calculate_self_similarity()` returns the closest self
match and a boolean `is_self_like` flag.

**Anchor residue quality (anchor hydrophobicity).** MHC anchor positions strongly
influence binding stability. For HLA-A\*02:01 and similar alleles:
- Position 2 prefers hydrophobic residues (L, I, V, M)
- C-terminal position prefers hydrophobic residues (L, V, I)

`check_anchor_hydrophobicity()` returns per-position hydrophobicity booleans and a
0-2 `anchor_score`. Strong anchors improve binding and are a quality signal when
classifying anchor vs. non-anchor positions in a peptide.

**Ranking & tiering.** `rank_epitopes()` scores all candidates, sorts by composite
immunogenicity, and assigns confidence tiers:
- **High**: top 5%, all factors favorable
- **Medium**: top 20%, most factors favorable
- **Low**: remaining, some factors favorable

**Vaccine-candidate selection.** `compare_vaccine_candidates()` selects a diverse
set (typically 5-20) with broad HLA coverage and non-overlapping positions, so the
chosen epitopes are not redundant.

## Mutation & neoantigen impact

Predict mutant peptides from somatic variants that bind patient HLA alleles and may
elicit T-cell responses for personalized cancer immunotherapy.

**pVACtools pipeline.** The standard route is `pvacseq run` on a VEP-annotated VCF
plus patient HLA types. Install via pip (optionally a dedicated conda env) and
`pvactools download_iedb_tools` for the IEDB prediction engine. Key parameters:
- `-e1` epitope lengths for MHC-I (8-11), `-e2` for MHC-II (15)
- `--binding-threshold` IC50 cutoff (default 500), or `--percentile-threshold`
- `--iedb-install-directory` path to the IEDB tools

**VCF annotation requirements.** pVACseq requires a VEP-annotated VCF carrying
transcript consequences and amino-acid changes. Run Ensembl VEP first with the
`Downstream` and `Wildtype` plugins (`--terms SO --symbol`).

**Agretopicity (Differential Agretopicity Index, DAI).** The ratio of WT to MT
binding identifies mutations that create new epitopes:
- **> 1**: mutant binds better (favorable — mutation creates an epitope)
- **~ 1**: similar binding (less likely immunogenic)
- **< 1**: WT binds better (unfavorable)

`calculate_agretopicity()` adds an `agretopicity` column and a `dai_favorable`
boolean to parsed pVACseq results.

**Binder classification by DAI.** Use the favorable/neutral/unfavorable split above
as the mechanism call: a high-DAI mutant is a newly-created (improved) binder; a
DAI near 1 is a conserved binder where the mutation does not change presentation; a
DAI below 1 is a disrupted binder. This mechanism distinction is what separates a
true neoepitope from a passenger mutation.

**Prioritization decision table.** `prioritize_neoantigens()` applies sequential
filters then a composite score. Good candidates satisfy:

| Criterion | Threshold | Rationale |
|-----------|-----------|-----------|
| MHC binding | IC50 < 500 nM (ideally < 50 nM) | Must be presented |
| Agretopicity | DAI > 1 | Mutation creates/improves epitope |
| Clonality | Tumor DNA VAF ≥ 0.1 | Present in most tumor cells |
| Expression | Gene expression ≥ 1.0 TPM | Unexpressed genes won't present |
| Foreignness | not in tolerogenic region | Avoid self-tolerance |

Priority score = (1 / Median MT Score) × VAF × agretopicity. A typical pipeline
returns 10-50 candidates per patient.

**Manual pipeline (no pVACtools).** Parse VEP annotations from a VCF with `cyvcf2`,
generate mutant peptides around each coding mutation, and predict binding with
MHCflurry's `Class1PresentationPredictor` — see `manual_neoantigen_pipeline()`.

**Quality metrics.** `assess_neoantigen_quality()` normalizes binding,
agretopicity, clonality, and expression to 0-1 and combines them with weights
(binding 0.3, agretopicity 0.3, clonality 0.2, expression 0.2) into a composite
confidence score.

## TCR recognition prediction

The final stage: does a T-cell receptor actually recognize the prioritized
epitope? CDR3 beta is the primary determinant of specificity; the alpha chain adds
roughly 20% additional accuracy.

**ERGO-II.** A deep-learning model for TCR-epitope binding that uses both CDR3
alpha and beta chains, incorporates MHC context, and is trained on VDJdb and IEDB
data. Setup: clone `https://github.com/IdoSpringer/ERGO-II`, `pip install torch
pandas scikit-learn`, and download the pre-trained models from the repository.
ERGO-II gives better predictions than simple heuristics or database matching.

**TCR input format.** `parse_tcr_data()` reads a TSV and validates CDR3 sequences
against the 20 amino acids. Required/optional columns:
- `cdr3_beta` (required, most informative)
- `cdr3_alpha` (optional, improves accuracy)
- `v_beta`, `j_beta` (optional V/J gene usage)

**Heuristic compatibility.** When ERGO-II is unavailable, `predict_binding_simple()`
gives a rough score from CDR3-length compatibility and charge complementarity
(opposite charges suggest binding). Treat this as a fallback only.

**Database matching.** `match_to_vdjdb()` matches query CDR3s against VDJdb
(curated TCR-epitope pairs, https://vdjdb.cdr3.net/) by exact match or fuzzy match
(>90% similarity / edit distance ≤ 1).

**Clustering.** `cluster_tcrs_by_specificity()` computes pairwise Levenshtein
distances, applies hierarchical clustering (average linkage), and cuts at a max
edit distance (default 3) to define specificity groups. TCRs within 1-3 edits often
share specificity.

**Repertoire specificity.** `analyze_repertoire_specificity()` reports the fraction
of a repertoire matching known epitopes, epitope diversity, and potential public
TCRs shared across individuals.

## PepX expression annotation

PepX (IEDB) annotates peptides with gene/transcript expression from public
datasets, providing the **expression factor** for immunogenicity scoring.

**Prerequisites:**
- PepX standalone: `~/iedb-tools/pepx/` (IEDB_NG_PEPX-0.1.1-beta)
- PepX SQLite database (~120 GB): `~/iedb-tools/pepx/download_pepx_db.sh`
- `PEPX_DB_PATH` env var pointing at the database

**Datasets** — gene-level: Abelin, CCLE, GTEx, HeLa, TCGA; transcript-level: CCLE,
GTEx, HPA, TCGA.

**API.** `pepx_lookup()` queries the `peptide_{gene,transcript}_tpms` table for a
list of peptides in a dataset; `pepx_list_datasets()` enumerates available datasets;
`add_expression_from_pepx()` joins TPM values onto an epitope DataFrame as the
`expression_tpm` column consumed by `calculate_immunogenicity_score()`. Full code,
including the local-setup verification snippet, is in the Usage Guide.

**Where PepX fits in the MHC-II / immunogenicity pipeline.** Two integration points:

1. **Post-MHC-II binding** — after NetMHCIIpan predicts binders, annotate with
   expression. Peptides from non-expressed source proteins in APC-relevant tissues
   are lower risk.
2. **Post-pathogen mimicry** — after a homology search finds pathogen homologs,
   annotate hits with expression. A mimicry hit against a non-expressed human
   protein is less concerning.

## Common errors

- **`PEPX_DB_PATH not set`** — export the env var or pass `db_path` explicitly. The
  ~120 GB DB must be downloaded first via `download_pepx_db.sh` (>1 hour).
- **pVACseq rejects the VCF** — the VCF must be VEP-annotated with `Downstream` +
  `Wildtype` plugins; a plain somatic VCF lacks the amino-acid-change fields.
- **Empty PepX result** — peptides must be uppercased and present in the chosen
  dataset; `add_expression_from_pepx()` fills missing rows with `expression_tpm=0`.
- **ImportError on MHCflurry models** — run `mhcflurry-downloads fetch` once before
  loading any predictor.
- **ERGO-II import/model errors** — models are not pip-installed; clone the repo and
  download weights separately. Fall back to `predict_binding_simple()` or VDJdb
  matching if unavailable.
- **Self-similarity returns 0 for all** — `calculate_self_similarity()` compares
  equal-length sequences only; ensure the proteome peptide set matches candidate
  length.
- **Version drift** — if any example raises ImportError/AttributeError/TypeError,
  introspect the installed package (`pip show`, `help()`) and adapt rather than
  retrying.

## Related Skills

- `bio-immunoinformatics` (mhc-binding-prediction reference) — binding-affinity component (input)
- `bio-immunoinformatics` (epitope-prediction reference) — epitope identification (upstream)
- `bio-database-access-uniprot-access` — proteome peptides for self-similarity
- `bio-database-access-geo-data` — alternative expression source (GEO)

---

# Usage Guide

## Overview

A T-cell immunogenicity decision pipeline: score epitope immunogenicity → assess
mutation/neoantigen impact → predict TCR recognition. This guide holds the runnable
reference code; the section above is the navigable overview.

## Prerequisites

```bash
# Stage 1 — immunogenicity scoring
pip install mhcflurry pandas numpy
mhcflurry-downloads fetch

# Stage 2 — neoantigen prediction (dedicated env recommended)
conda create -n pvactools python=3.8
conda activate pvactools
pip install pvactools
pvactools download_iedb_tools

# Stage 3 — TCR recognition
pip install pandas torch scikit-learn scipy
# ERGO-II: git clone https://github.com/IdoSpringer/ERGO-II
```

## Quick Start

Tell your AI agent what you want to do:
- "Rank these neoantigens by immunogenicity"
- "Find neoantigens from my somatic VCF for this patient's HLA type"
- "Predict what antigens this TCR sequence recognizes"
- "Match my TCRs to known epitopes in VDJdb"

## Example Prompts

### Immunogenicity scoring
> "Calculate immunogenicity scores for these peptides"
> "Which neoantigens are most likely to be immunogenic?"
> "Select top 20 vaccine candidates from my list"
> "Check if these peptides are self-like"
> "Assess anchor residue preferences for HLA-A*02:01"

### Neoantigen prediction
> "Run neoantigen prediction on my annotated VCF"
> "Find clonal neoantigens with high expression"
> "Design a personalized cancer vaccine from these mutations"

### TCR recognition
> "What epitopes might this CDR3 beta sequence recognize?"
> "Find matches for my TCRs in VDJdb"
> "Cluster TCRs that likely share specificity"

## What the Agent Will Do

1. Score MHC binding, processing, expression, clonality, and foreignness.
2. Combine factors into a weighted immunogenicity score; rank and tier.
3. For tumors: verify VEP annotation, run pVACseq, compute agretopicity, prioritize.
4. For TCRs: parse CDR3 data, match to VDJdb, run ERGO-II, cluster by specificity.

## Tips

- **Binding weight** — usually highest (25-35% of score).
- **Agretopicity** — important for neoantigens (MT vs WT, DAI > 1 favorable).
- **Self-similarity** — high similarity suggests tolerance; lower foreignness.
- **Expression** — unexpressed antigens won't be presented.
- **VEP annotation** — required for amino-acid-change information in neoantigen work.
- **HLA typing** — use patient-specific alleles (6 alleles typical).
- **CDR3 beta** — most informative for TCR specificity; alpha adds ~20%.
- **Validation** — predicted TCR specificities should be validated experimentally.

---

# Stage 1 — T-cell immunogenicity scoring

## Multi-factor scoring

Calculate a composite immunogenicity score from weighted factors (binding,
agretopicity, processing, expression, clonality, foreignness). Each factor is
scored 0-1, then combined via a weighted sum.

```python
import pandas as pd
import numpy as np

def calculate_immunogenicity_score(peptide_data):
    '''Calculate composite immunogenicity score

    Factors considered:
    1. MHC binding affinity (IC50)
    2. Agretopicity (MT vs WT binding ratio)
    3. Proteasomal processing
    4. TAP transport
    5. Expression level
    6. Clonality (VAF for neoantigens)
    7. Self-similarity (avoid tolerance)

    Each factor scored 0-1, then weighted and combined.
    '''
    scores = {}

    # 1. Binding affinity (lower IC50 = better)
    # Transform to 0-1: 1 at 0nM, 0 at 5000nM
    ic50 = peptide_data.get('ic50_nM', 500)
    scores['binding'] = 1 - min(ic50 / 5000, 1)

    # 2. Agretopicity (MT binds better than WT)
    # Ratio of WT/MT IC50, capped at 10
    agretopicity = peptide_data.get('agretopicity', 1.0)
    scores['agretopicity'] = min(agretopicity / 10, 1)

    # 3. Processing score (from MHCflurry)
    processing = peptide_data.get('processing_score', 0.5)
    scores['processing'] = processing

    # 4. Expression (log scale, capped)
    expression = peptide_data.get('expression_tpm', 10)
    scores['expression'] = min(np.log10(expression + 1) / 3, 1)

    # 5. Clonality (for neoantigens)
    vaf = peptide_data.get('vaf', 0.5)
    scores['clonality'] = vaf

    # 6. Self-similarity (lower = better, less tolerance)
    self_sim = peptide_data.get('self_similarity', 0.5)
    scores['foreignness'] = 1 - self_sim

    # Weighted combination
    weights = {
        'binding': 0.25,
        'agretopicity': 0.20,
        'processing': 0.10,
        'expression': 0.15,
        'clonality': 0.15,
        'foreignness': 0.15
    }

    total = sum(scores[k] * weights[k] for k in weights)

    return total, scores
```

## Processing prediction

Predict proteasomal cleavage and TAP transport probability via MHCflurry's
`Class1ProcessingPredictor`.

```python
from mhcflurry import Class1ProcessingPredictor

def predict_processing_score(peptides):
    '''Predict proteasomal cleavage and TAP transport

    Processing score reflects probability that peptide will be:
    1. Cleaved from protein by proteasome
    2. Transported by TAP into ER
    3. Loaded onto MHC

    Higher processing score = more likely to be presented
    '''
    predictor = Class1ProcessingPredictor.load()

    results = []
    for peptide in peptides:
        # Need surrounding sequence context for processing
        # In practice, extract from protein context
        pred = predictor.predict(peptides=[peptide])
        results.append({
            'peptide': peptide,
            'processing_score': pred['processing_score'].values[0]
        })

    return pd.DataFrame(results)
```

## Self-similarity assessment

Determine whether a candidate resembles self-peptides (potential T-cell tolerance).

```python
def calculate_self_similarity(peptide, proteome_peptides, threshold=0.8):
    '''Check if peptide is similar to self-peptides

    High similarity to self-peptides suggests:
    - T-cells may be tolerized (deleted during development)
    - Lower likelihood of immune response

    Threshold 0.8 = 80% identity considered "self-like"
    '''
    def sequence_identity(seq1, seq2):
        if len(seq1) != len(seq2):
            return 0
        matches = sum(1 for a, b in zip(seq1, seq2) if a == b)
        return matches / len(seq1)

    max_similarity = 0
    most_similar = None

    for self_peptide in proteome_peptides:
        sim = sequence_identity(peptide, self_peptide)
        if sim > max_similarity:
            max_similarity = sim
            most_similar = self_peptide

    return {
        'similarity': max_similarity,
        'is_self_like': max_similarity >= threshold,
        'closest_self': most_similar
    }
```

## Anchor hydrophobicity (anchor residue quality)

Assess MHC anchor residue quality at key positions.

```python
def check_anchor_hydrophobicity(peptide):
    '''Check hydrophobicity at MHC anchor positions

    For HLA-A*02:01 and similar alleles:
    - Position 2: Prefers hydrophobic (L, I, V, M)
    - Position 9 (C-terminus): Prefers hydrophobic (L, V, I)

    Strong anchors improve binding stability.
    '''
    hydrophobic = set('LIVMFYW')

    pos2 = peptide[1] if len(peptide) > 1 else ''
    pos_last = peptide[-1]

    return {
        'pos2_hydrophobic': pos2 in hydrophobic,
        'pos_last_hydrophobic': pos_last in hydrophobic,
        'anchor_score': (pos2 in hydrophobic) + (pos_last in hydrophobic)
    }
```

## Rank epitopes (scoring + tiering)

```python
def rank_epitopes(epitope_df, top_n=20):
    '''Rank epitopes by immunogenicity

    Returns top candidates with scores and confidence tiers.

    Confidence tiers:
    - High: Top 5%, all factors favorable
    - Medium: Top 20%, most factors favorable
    - Low: Remaining, some factors favorable
    '''
    epitope_df = epitope_df.copy()

    # Calculate scores
    scores = []
    factor_scores = []
    for _, row in epitope_df.iterrows():
        total, factors = calculate_immunogenicity_score(row.to_dict())
        scores.append(total)
        factor_scores.append(factors)

    epitope_df['immunogenicity_score'] = scores
    factor_df = pd.DataFrame(factor_scores)

    # Combine
    result = pd.concat([epitope_df, factor_df], axis=1)

    # Rank
    result = result.sort_values('immunogenicity_score', ascending=False)

    # Assign tiers
    n = len(result)
    result['tier'] = 'low'
    result.iloc[:int(n * 0.20), result.columns.get_loc('tier')] = 'medium'
    result.iloc[:int(n * 0.05), result.columns.get_loc('tier')] = 'high'

    return result.head(top_n)
```

## Compare / select vaccine candidates

```python
def compare_vaccine_candidates(candidates_df):
    '''Compare and select vaccine candidates

    Vaccine design typically selects:
    - Multiple epitopes (5-20)
    - Diverse HLA coverage
    - High immunogenicity scores
    - Non-overlapping sequences
    '''
    # Group by HLA coverage
    hla_coverage = candidates_df.groupby('allele').size()

    # Select diverse set
    selected = []
    used_positions = set()

    for _, candidate in candidates_df.iterrows():
        # Check for overlap with selected
        pos = candidate.get('position', 0)
        if not any(abs(pos - p) < 5 for p in used_positions):
            selected.append(candidate)
            used_positions.add(pos)

        if len(selected) >= 20:
            break

    return pd.DataFrame(selected)
```

---

# Stage 2 — Mutation & neoantigen impact

## pVACtools install

```bash
# Install pVACtools
pip install pvactools

# Or use conda for dependencies
conda create -n pvactools python=3.8
conda activate pvactools
pip install pvactools

# Download IEDB tools
pvactools download_iedb_tools
```

## pVACseq workflow

```bash
# Run pVACseq on annotated VCF
pvacseq run \
    annotated.vcf \
    sample_name \
    "HLA-A*02:01,HLA-A*24:02,HLA-B*07:02,HLA-B*44:02" \
    MHCflurry MHCnuggetsI \
    output_dir \
    -e1 8,9,10,11 \
    --iedb-install-directory /path/to/iedb

# Key parameters:
# -e1: Epitope lengths for MHC-I (8-11)
# -e2: Epitope lengths for MHC-II (15)
# --binding-threshold: IC50 cutoff (default 500)
# --percentile-threshold: Alternative cutoff
```

## VCF annotation requirements

```bash
# pVACseq requires VEP-annotated VCF
# Must include transcript and amino acid changes

# Run VEP first
vep -i somatic.vcf -o annotated.vcf \
    --cache --offline \
    --format vcf --vcf \
    --plugin Downstream \
    --plugin Wildtype \
    --terms SO \
    --symbol
```

## Parse pVACseq results + agretopicity (DAI)

```python
import pandas as pd

def parse_pvacseq_results(results_file):
    '''Parse pVACseq output

    Key columns:
    - Mutation: Gene and amino acid change
    - HLA Allele: Patient HLA presenting this peptide
    - MT Epitope Seq: Mutant peptide sequence
    - WT Epitope Seq: Wild-type peptide sequence
    - Median MT Score: Binding affinity (nM)
    - Median WT Score: WT binding (for agretopicity)
    - Tumor DNA VAF: Variant allele frequency
    - Gene Expression: If RNA-seq available
    '''
    df = pd.read_csv(results_file, sep='\t')

    # Filter by binding threshold
    strong_binders = df[df['Median MT Score'] < 500]

    return strong_binders


def calculate_agretopicity(df):
    '''Calculate agretopicity (DAI) score

    Agretopicity = ratio of WT to MT binding
    Higher agretopicity means MT binds better than WT
    indicating mutation creates new epitope

    DAI (Differential Agretopicity Index):
    - >1: Mutant binds better (favorable)
    - ~1: Similar binding (less likely immunogenic)
    - <1: WT binds better (unfavorable)
    '''
    df = df.copy()
    df['agretopicity'] = df['Median WT Score'] / df['Median MT Score']

    # High agretopicity = mutation improves binding
    df['dai_favorable'] = df['agretopicity'] > 1

    return df
```

## Prioritize neoantigens (decision table → score)

```python
def prioritize_neoantigens(df, vaf_threshold=0.1, expression_threshold=1.0):
    '''Prioritize neoantigens for vaccine design

    Criteria for good neoantigen candidates:
    1. Strong MHC binding (IC50 < 500nM, ideally < 50nM)
    2. High agretopicity (MT binds better than WT)
    3. High tumor VAF (clonal, present in most tumor cells)
    4. Expressed in tumor (if RNA-seq available)
    5. Not in tolerogenic region (self-similarity check)

    Typical pipeline returns 10-50 candidates per patient
    '''
    candidates = df.copy()

    # Filter by binding
    candidates = candidates[candidates['Median MT Score'] < 500]

    # Filter by VAF (clonal mutations preferred)
    if 'Tumor DNA VAF' in candidates.columns:
        candidates = candidates[candidates['Tumor DNA VAF'] >= vaf_threshold]

    # Filter by expression
    if 'Gene Expression' in candidates.columns:
        candidates = candidates[candidates['Gene Expression'] >= expression_threshold]

    # Calculate priority score
    # Lower binding affinity = better
    # Higher VAF = better
    # Higher agretopicity = better
    candidates['priority_score'] = (
        (1 / candidates['Median MT Score']) *
        candidates.get('Tumor DNA VAF', 1) *
        candidates.get('agretopicity', 1)
    )

    return candidates.sort_values('priority_score', ascending=False)
```

## Manual neoantigen pipeline (no pVACtools)

```python
def manual_neoantigen_pipeline(vcf_file, hla_alleles, reference_fasta):
    '''Simplified neoantigen prediction without pVACtools

    Steps:
    1. Extract coding mutations from VCF
    2. Generate mutant protein sequences
    3. Extract peptides around mutation
    4. Predict MHC binding
    '''
    from cyvcf2 import VCF
    from mhcflurry import Class1PresentationPredictor

    vcf = VCF(vcf_file)
    predictor = Class1PresentationPredictor.load()

    neoantigens = []

    for variant in vcf:
        # Get amino acid change from VEP annotation
        if 'CSQ' not in variant.INFO:
            continue

        # Parse consequence and extract mutant peptides
        # ... (implementation depends on annotation format)

        # For each mutant peptide, predict binding
        for peptide in mutant_peptides:
            for allele in hla_alleles:
                pred = predictor.predict(peptides=[peptide], alleles=[allele])
                if pred['mhcflurry_affinity'].values[0] < 500:
                    neoantigens.append({
                        'variant': f'{variant.CHROM}:{variant.POS}',
                        'peptide': peptide,
                        'allele': allele,
                        'affinity': pred['mhcflurry_affinity'].values[0]
                    })

    return neoantigens
```

## Neoantigen quality metrics

```python
def assess_neoantigen_quality(neoantigen):
    '''Assess multiple quality metrics for neoantigen

    Returns composite quality score considering:
    - Binding affinity
    - Agretopicity
    - Clonality (VAF)
    - Expression
    - Self-similarity
    '''
    scores = {}

    # Binding (0-1, lower IC50 = higher score)
    ic50 = neoantigen.get('Median MT Score', 500)
    scores['binding'] = 1 - min(ic50 / 5000, 1)

    # Agretopicity (0-1)
    dai = neoantigen.get('agretopicity', 1)
    scores['agretopicity'] = min(dai / 10, 1)

    # Clonality (0-1)
    vaf = neoantigen.get('Tumor DNA VAF', 0.5)
    scores['clonality'] = vaf

    # Expression (0-1, log scale)
    import math
    expr = neoantigen.get('Gene Expression', 1)
    scores['expression'] = min(math.log10(expr + 1) / 3, 1)

    # Composite score
    weights = {'binding': 0.3, 'agretopicity': 0.3, 'clonality': 0.2, 'expression': 0.2}
    composite = sum(scores[k] * weights[k] for k in weights)

    return composite, scores
```

---

# Stage 3 — TCR recognition prediction

## ERGO-II setup

```python
# ERGO-II uses deep learning to predict TCR-epitope binding
# GitHub: https://github.com/IdoSpringer/ERGO-II

def setup_ergo():
    '''Setup ERGO-II for TCR-epitope prediction

    Requirements:
    - PyTorch
    - Pre-trained models from ERGO-II repository

    ERGO-II features:
    - Uses both CDR3 alpha and beta chains
    - Incorporates MHC context
    - Trained on VDJdb and IEDB data
    '''
    print('ERGO-II setup:')
    print('1. Clone: git clone https://github.com/IdoSpringer/ERGO-II')
    print('2. Install: pip install torch pandas scikit-learn')
    print('3. Download models from repository')
```

## TCR input format

```python
def parse_tcr_data(tcr_file):
    '''Parse TCR sequence data

    Required columns:
    - cdr3_beta: CDR3 beta chain sequence (most informative)
    - cdr3_alpha: CDR3 alpha chain (optional, improves accuracy)
    - v_beta: V gene usage (optional)
    - j_beta: J gene usage (optional)

    CDR3 is the primary determinant of antigen specificity.
    Alpha chain provides ~20% additional specificity.
    '''
    import pandas as pd

    df = pd.read_csv(tcr_file, sep='\t')

    # Validate CDR3 sequences
    valid_aa = set('ACDEFGHIKLMNPQRSTVWY')

    def is_valid_cdr3(seq):
        if pd.isna(seq):
            return False
        return all(aa in valid_aa for aa in seq.upper())

    df['valid_beta'] = df['cdr3_beta'].apply(is_valid_cdr3)

    return df[df['valid_beta']]
```

## Heuristic compatibility (ERGO-II fallback)

```python
def predict_binding_simple(cdr3_beta, epitope):
    '''Simple TCR-epitope compatibility score

    This is a simplified heuristic. For accurate predictions,
    use ERGO-II or other deep learning models.

    Features considered:
    - CDR3 length compatibility
    - Amino acid composition
    - Hydrophobicity matching
    '''
    # Length compatibility
    # TCRs recognizing similar epitopes often have similar CDR3 lengths
    optimal_length = len(epitope) + 5  # Rough heuristic
    length_score = 1 - abs(len(cdr3_beta) - optimal_length) / 10

    # Charge complementarity
    positive = set('RKH')
    negative = set('DE')

    tcr_charge = sum(1 if aa in positive else -1 if aa in negative else 0
                    for aa in cdr3_beta)
    epitope_charge = sum(1 if aa in positive else -1 if aa in negative else 0
                        for aa in epitope)

    # Opposite charges suggest complementarity
    charge_score = 0.5 + (tcr_charge * -epitope_charge) / 20

    return {
        'cdr3_beta': cdr3_beta,
        'epitope': epitope,
        'length_score': max(0, min(1, length_score)),
        'charge_score': max(0, min(1, charge_score)),
        'combined': (length_score + charge_score) / 2
    }
```

## Match TCRs to known epitopes (VDJdb)

```python
def match_to_vdjdb(tcr_sequences, vdjdb_path='vdjdb.tsv'):
    '''Match TCRs to known epitopes in VDJdb

    VDJdb is a curated database of TCR-epitope pairs.
    Download from: https://vdjdb.cdr3.net/

    Matching approaches:
    - Exact CDR3 match
    - Similar CDR3 (edit distance ≤1)
    - Cluster-based (group similar TCRs)
    '''
    import pandas as pd
    from difflib import SequenceMatcher

    vdjdb = pd.read_csv(vdjdb_path, sep='\t')

    matches = []
    for tcr in tcr_sequences:
        # Exact match
        exact = vdjdb[vdjdb['cdr3'] == tcr]
        if len(exact) > 0:
            matches.append({
                'query_tcr': tcr,
                'match_type': 'exact',
                'epitopes': exact['antigen.epitope'].tolist(),
                'species': exact['antigen.species'].tolist()
            })
            continue

        # Fuzzy match (1 mismatch)
        for _, row in vdjdb.iterrows():
            similarity = SequenceMatcher(None, tcr, row['cdr3']).ratio()
            if similarity > 0.9:  # >90% similar
                matches.append({
                    'query_tcr': tcr,
                    'match_type': 'similar',
                    'similarity': similarity,
                    'db_tcr': row['cdr3'],
                    'epitope': row['antigen.epitope'],
                    'species': row['antigen.species']
                })

    return pd.DataFrame(matches)
```

## TCR clustering by specificity

Group TCRs that likely recognize the same epitope from CDR3 sequence similarity:
compute pairwise Levenshtein distances, apply hierarchical clustering with average
linkage, and cut the dendrogram at a maximum edit-distance threshold.

```python
def cluster_tcrs_by_specificity(tcr_sequences, method='levenshtein'):
    '''Cluster TCRs likely to share specificity

    TCRs recognizing the same epitope often have:
    - Similar CDR3 length
    - Shared motifs
    - Similar V gene usage

    Methods:
    - levenshtein: Edit distance clustering
    - tcrdist: TCRdist3 distance metric
    - deep: Deep learning embeddings
    '''
    from scipy.cluster.hierarchy import linkage, fcluster
    from scipy.spatial.distance import pdist, squareform
    import numpy as np

    def levenshtein_distance(s1, s2):
        if len(s1) < len(s2):
            return levenshtein_distance(s2, s1)
        if len(s2) == 0:
            return len(s1)

        previous_row = range(len(s2) + 1)
        for i, c1 in enumerate(s1):
            current_row = [i + 1]
            for j, c2 in enumerate(s2):
                insertions = previous_row[j + 1] + 1
                deletions = current_row[j] + 1
                substitutions = previous_row[j] + (c1 != c2)
                current_row.append(min(insertions, deletions, substitutions))
            previous_row = current_row

        return previous_row[-1]

    # Calculate pairwise distances
    n = len(tcr_sequences)
    distances = np.zeros((n, n))
    for i in range(n):
        for j in range(i + 1, n):
            d = levenshtein_distance(tcr_sequences[i], tcr_sequences[j])
            distances[i, j] = distances[j, i] = d

    # Cluster
    condensed = squareform(distances)
    Z = linkage(condensed, method='average')
    clusters = fcluster(Z, t=3, criterion='distance')  # Max 3 edits

    return dict(zip(tcr_sequences, clusters))
```

## Analyze repertoire specificity

```python
def analyze_repertoire_specificity(tcr_df, epitope_db):
    '''Analyze antigen specificity of TCR repertoire

    Reports:
    - Fraction matching known epitopes
    - Epitope diversity
    - Potential public TCRs (shared across individuals)
    '''
    results = {
        'total_tcrs': len(tcr_df),
        'unique_cdr3': tcr_df['cdr3_beta'].nunique(),
        'matched_epitopes': 0,
        'epitope_distribution': {}
    }

    # Match to database
    matched = match_to_vdjdb(tcr_df['cdr3_beta'].unique(), epitope_db)

    if len(matched) > 0:
        results['matched_epitopes'] = len(matched['query_tcr'].unique())
        results['epitope_distribution'] = matched['epitope'].value_counts().to_dict()

    return results
```

---

# PepX expression annotation (IEDB)

Annotate peptides with gene/transcript expression from public datasets. Provides
the expression factor for `calculate_immunogenicity_score()`.

**Prerequisites:**
- PepX standalone: `~/iedb-tools/pepx/` (IEDB_NG_PEPX-0.1.1-beta)
- PepX SQLite database (~120GB): download via `~/iedb-tools/pepx/download_pepx_db.sh`
- Set `PEPX_DB_PATH` env var to the database location

**Available datasets** (gene-level): Abelin, CCLE, GTEx, HeLa, TCGA
**Available datasets** (transcript-level): CCLE, GTEx, HPA, TCGA

## PepX lookup

```python
import os
import sys
import sqlite3
import pandas as pd

def pepx_lookup(
    peptides: list[str],
    dataset_id: str,
    quantification: str = "gene",
    db_path: str | None = None,
) -> pd.DataFrame:
    """Look up peptide expression via PepX SQLite database.

    Args:
        peptides: List of peptide sequences (e.g. ["FVQMMTAK", "MRYVASYL"]).
        dataset_id: Dataset ID from PepX (use pepx_list_datasets to find).
        quantification: "gene" or "transcript".
        db_path: Path to PepX SQLite DB. Falls back to PEPX_DB_PATH env var.

    Returns:
        DataFrame with peptide, gene symbols, TPM values, protein fractions.
    """
    db_path = db_path or os.environ.get("PEPX_DB_PATH")
    if not db_path:
        raise ValueError(
            "PepX database path not set. Either pass db_path or set PEPX_DB_PATH. "
            "Download the DB via: ~/iedb-tools/pepx/download_pepx_db.sh"
        )

    conn = sqlite3.connect(db_path)
    table = f"peptide_{quantification}_tpms"

    placeholders = ",".join("?" for _ in peptides)
    query = f"SELECT * FROM {table} WHERE dataset_id = ? AND peptide IN ({placeholders})"
    params = [dataset_id] + [p.upper() for p in peptides]

    cursor = conn.cursor()
    pragmas = cursor.execute(f"PRAGMA table_info({table})").fetchall()
    columns = [row[1] for row in pragmas]

    cursor.execute(query, params)
    rows = cursor.fetchall()
    conn.close()

    return pd.DataFrame(rows, columns=columns)


def pepx_list_datasets(
    quantification: str = "gene",
    source: str | None = None,
    db_path: str | None = None,
) -> pd.DataFrame:
    """List available PepX expression datasets.

    Args:
        quantification: "gene" or "transcript".
        source: Filter by source (e.g. "CCLE", "GTEx", "TCGA"). None for all.
        db_path: Path to PepX SQLite DB. Falls back to PEPX_DB_PATH env var.
    """
    db_path = db_path or os.environ.get("PEPX_DB_PATH")
    if not db_path:
        raise ValueError("PEPX_DB_PATH not set")

    conn = sqlite3.connect(db_path)
    query = (
        "SELECT dataset_id, source, title, n_samples "
        "FROM expression_dataset WHERE {q}_level_data IS TRUE"
    ).format(q=quantification)
    if source:
        query += f" AND source = '{source}'"

    df = pd.read_sql_query(query, conn)
    conn.close()
    return df
```

## Integrating PepX with immunogenicity scoring

```python
def add_expression_from_pepx(
    epitope_df: pd.DataFrame,
    peptide_column: str = "peptide",
    dataset_id: str = "329",
    quantification: str = "gene",
) -> pd.DataFrame:
    """Annotate an epitope DataFrame with PepX expression TPM values.

    Adds 'expression_tpm' column used by calculate_immunogenicity_score().

    Args:
        epitope_df: DataFrame with a peptide column.
        peptide_column: Name of the column containing peptide sequences.
        dataset_id: PepX dataset ID (use pepx_list_datasets to find).
        quantification: "gene" or "transcript".

    Returns:
        Input DataFrame with 'expression_tpm' and 'gene_symbol' columns added.
    """
    peptides = epitope_df[peptide_column].unique().tolist()
    pepx_df = pepx_lookup(peptides, dataset_id, quantification)

    if pepx_df.empty:
        epitope_df["expression_tpm"] = 0.0
        epitope_df["gene_symbol"] = ""
        return epitope_df

    # PepX collapsed table has max TPM per peptide across matched genes
    tpm_col = [c for c in pepx_df.columns if "tpm" in c.lower() and "max" in c.lower()]
    gene_col = [c for c in pepx_df.columns if "gene_symbol" in c.lower()]

    agg = pepx_df.groupby("peptide").agg(
        expression_tpm=(tpm_col[0] if tpm_col else pepx_df.columns[-1], "max"),
        gene_symbol=(gene_col[0] if gene_col else "peptide", "first"),
    ).reset_index()

    return epitope_df.merge(agg, left_on=peptide_column, right_on="peptide", how="left").fillna(
        {"expression_tpm": 0.0, "gene_symbol": ""}
    )
```

## Use in MHC-II / immunogenicity pipeline

PepX expression annotation fits at two points in the MHC-II / immunogenicity pipeline:

1. **Post-MHC-II binding**: After NetMHCIIpan predicts binders, annotate with expression.
   Peptides from non-expressed source proteins in APC-relevant tissues are lower risk.

2. **Post-pathogen mimicry**: After a homology search finds pathogen homologs, annotate
   hits with expression data. A mimicry hit against a non-expressed human protein is
   less concerning.

```python
# Example: annotate MHC-II binders with expression
binding_results = pd.read_csv("mhcii_predictions.tsv", sep="\t")
annotated = add_expression_from_pepx(binding_results, peptide_column="Peptide")
# Now 'expression_tpm' feeds into calculate_immunogenicity_score()
```

## PepX local setup

```bash
# PepX standalone lives outside the repo alongside other IEDB tools
# Location: ~/iedb-tools/pepx/

# 1. Download the database (~120GB, takes >1 hour)
cd ~/iedb-tools/pepx && ./download_pepx_db.sh

# 2. Set env var (add to .bashrc or .claude/settings.json)
export PEPX_DB_PATH="$HOME/iedb-tools/pepx/pepX-prod-20231018.indexed.sqlite"

# 3. Verify
python3 -c "
import sqlite3, os
conn = sqlite3.connect(os.environ['PEPX_DB_PATH'])
print('Tables:', [r[0] for r in conn.execute(\"SELECT name FROM sqlite_master WHERE type='table'\").fetchall()])
conn.close()
"
```
