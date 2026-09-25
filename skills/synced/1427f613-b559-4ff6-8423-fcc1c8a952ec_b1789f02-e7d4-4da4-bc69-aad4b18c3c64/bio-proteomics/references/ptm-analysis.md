# PTM Analysis — Post-Translational Modifications

> Source skill: `bio-proteomics-ptm-analysis` (tool_type: mixed, primary_tool: pyOpenMS)

## Version Compatibility

Reference examples tested with: numpy 1.26+, pandas 2.2+, scipy 1.12+

Before using code patterns, verify installed versions match. If versions differ:
- Python: `pip show <package>` then `help(module.function)` to check signatures
- R: `packageVersion('<pkg>')` then `?function_name` to verify parameters

If code throws ImportError, AttributeError, or TypeError, introspect the installed
package and adapt the example to match the actual API rather than retrying.

# Post-Translational Modification Analysis

**"Analyze phosphorylation sites from my proteomics data"** → Identify and quantify post-translational modifications including phosphorylation, acetylation, and ubiquitination with site localization and motif analysis.
- Python: `pyopenms` for PTM-aware search, `scipy` for site-level statistics
- CLI: MaxQuant with variable modifications for enrichment-based PTM analysis

## Common PTMs and Mass Shifts

```python
PTM_MASSES = {
    'Phosphorylation': 79.966331,      # STY
    'Oxidation': 15.994915,             # M
    'Acetylation': 42.010565,           # K, N-term
    'Methylation': 14.015650,           # KR
    'Dimethylation': 28.031300,         # KR
    'Trimethylation': 42.046950,        # K
    'Ubiquitination': 114.042927,       # K (GlyGly remnant)
    'Deamidation': 0.984016,            # NQ
    'Carbamidomethyl': 57.021464,       # C (fixed mod from IAA)
}
```

## Processing MaxQuant PTM Output

**Goal:** Extract high-confidence phosphorylation sites from MaxQuant output with proper filtering and site annotation.

**Approach:** Load the Phospho(STY)Sites table, remove reverse hits and contaminants, filter by localization probability, and construct gene-level site identifiers.

```python
import pandas as pd
import numpy as np

# Phospho(STY)Sites.txt from MaxQuant
phospho = pd.read_csv('Phospho (STY)Sites.txt', sep='\t', low_memory=False)

# Filter valid sites
phospho = phospho[
    (phospho['Reverse'] != '+') &
    (phospho['Potential contaminant'] != '+')
]

# Filter by localization probability
phospho_confident = phospho[phospho['Localization prob'] >= 0.75]
print(f'Confident sites (prob >= 0.75): {len(phospho_confident)}')

# Extract site information
phospho_confident['site'] = phospho_confident.apply(
    lambda r: f"{r['Gene names']}_{r['Amino acid']}{r['Position']}", axis=1
)
```

## Site Localization Scoring

```python
def calculate_ascore_simple(peak_matches_with_ptm, peak_matches_without_ptm, total_peaks):
    '''Simplified A-score calculation'''
    if peak_matches_without_ptm >= peak_matches_with_ptm:
        return 0
    p = peak_matches_with_ptm / total_peaks if total_peaks > 0 else 0
    if p <= 0 or p >= 1:
        return 0

    from scipy.stats import binom
    p_value = 1 - binom.cdf(peak_matches_with_ptm - 1, total_peaks, 0.5)
    return -10 * np.log10(p_value) if p_value > 0 else 100
```

## Motif Analysis

```python
from collections import Counter

def extract_motifs(sites_df, sequence_col, position_col, window=7):
    '''Extract sequence windows around modification sites'''
    motifs = []
    for _, row in sites_df.iterrows():
        seq = row[sequence_col]
        pos = row[position_col] - 1  # 0-indexed
        start = max(0, pos - window)
        end = min(len(seq), pos + window + 1)

        # Pad if at sequence boundary
        motif = '_' * (window - (pos - start)) + seq[start:end] + '_' * (window - (end - pos - 1))
        motifs.append(motif)

    return motifs

def count_amino_acids_by_position(motifs, center=7):
    '''Count amino acid frequencies by position'''
    position_counts = {i: Counter() for i in range(-center, center + 1)}
    for motif in motifs:
        for i, aa in enumerate(motif):
            position_counts[i - center][aa] += 1
    return position_counts
```

## R: Site-Level Quantification with MSstatsPTM

```r
library(MSstatsPTM)

# Prepare input from MaxQuant
ptm_input <- MaxQtoMSstatsPTMFormat(
    evidence = read.table('evidence.txt', sep = '\t', header = TRUE),
    annotation = read.csv('annotation.csv'),
    fasta = 'uniprot_human.fasta',
    mod_type = 'Phospho'
)

# Process data
processed_ptm <- dataSummarizationPTM(ptm_input, method = 'msstats')

# Differential PTM analysis (adjusting for protein-level changes)
ptm_results <- groupComparisonPTM(processed_ptm, contrast.matrix = comparison_matrix)
```

---

## Usage Guide

### Overview
Identify, localize, and quantify post-translational modifications (phosphorylation, acetylation, ubiquitination, etc.) that regulate protein function.

### Prerequisites
```bash
pip install numpy pandas scipy
# R packages: BiocManager::install(c("MSstatsPTM", "PhosR"))
# CLI: MaxQuant (with PTM search), MSFragger
```

### Quick Start
Tell your AI agent what you want to do:
- "Analyze phosphorylation sites from my MaxQuant Phospho(STY)Sites.txt"
- "Find differentially regulated phosphosites between conditions"
- "Perform kinase enrichment analysis on my phosphoproteomics data"

### Example Prompts

#### Site Identification
> "Load the MaxQuant Phospho(STY)Sites.txt and filter to class I sites (localization probability > 0.75)"

> "Extract modification sites with confident localization from my search results"

> "Summarize PTM sites per protein and identify multiply-modified proteins"

#### Quantification
> "Normalize phosphosite intensities to total protein abundance"

> "Calculate site occupancy (modified / total) for each phosphorylation site"

> "Use MSstatsPTM to compare PTM levels adjusted for protein changes"

#### Differential Analysis
> "Find phosphosites changing significantly after drug treatment"

> "Identify acetylation sites regulated by the histone deacetylase inhibitor"

> "Compare ubiquitination profiles between wild-type and mutant cells"

#### Motif and Kinase Analysis
> "Run motif-x to find enriched sequence patterns around phosphosites"

> "Perform kinase enrichment analysis to identify active kinases"

> "Map my phosphosites to known kinase-substrate relationships"

### What the Agent Will Do
1. Load PTM site data from search engine output
2. Filter by localization probability (class I/II/III)
3. Normalize (optionally to protein level)
4. Perform differential analysis
5. Run motif/kinase enrichment
6. Generate site-level results and visualizations

### Common PTMs

| Modification | Sites | Mass Shift | Enrichment |
|--------------|-------|------------|------------|
| Phosphorylation | S, T, Y | +79.97 | TiO2, IMAC |
| Acetylation | K, N-term | +42.01 | Anti-acetyl antibody |
| Methylation | K, R | +14.02 | Anti-methyl antibody |
| Ubiquitination | K | +114.04 (GG) | Anti-K-GG antibody |
| Glycosylation | N, S, T | Variable | Lectin enrichment |

### Site Localization Classes
- **Class I (>0.75)**: Confident site assignment
- **Class II (0.50-0.75)**: Probable site
- **Class III (<0.50)**: Ambiguous site

### Tips
- Filter to class I sites for confident analysis
- Normalize to protein level to distinguish PTM changes from abundance changes
- Use MSstatsPTM for rigorous PTM vs protein statistical testing
- Check PhosphoSitePlus for known functions of regulated sites
