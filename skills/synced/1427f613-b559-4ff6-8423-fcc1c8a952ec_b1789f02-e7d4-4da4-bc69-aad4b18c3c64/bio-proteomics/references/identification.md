# Identification — Peptide ID & Protein Inference

> Source skill: `bio-proteomics-identification` (tool_type: mixed, primary_tool: pyOpenMS)

## Version Compatibility

Reference examples tested with: MSnbase 2.28+, pyOpenMS 3.1+

Before using code patterns, verify installed versions match. If versions differ:
- Python: `pip show <package>` then `help(module.function)` to check signatures
- R: `packageVersion('<pkg>')` then `?function_name` to verify parameters

If code throws ImportError, AttributeError, or TypeError, introspect the installed
package and adapt the example to match the actual API rather than retrying.

# Peptide & Protein Identification

**"Identify peptides from my MS/MS spectra and figure out which proteins are present"** → Match tandem mass spectra against a protein database to identify peptide-spectrum matches (PSMs), control false discovery rate with target-decoy competition, then group peptides into proteins using parsimony and apply protein-level FDR.
- Python: `pyopenms` for in-memory database search, PSM handling, and parsimony inference
- CLI: `comet`, `MSFragger`, `X!Tandem` for high-throughput database searching
- R: `MSnbase::readMzIdData()` / Bioconductor inference workflows for result processing

## Pipeline Context

This is the **middle stage** of the DDA chain:
`data-processing` → **identification** → `differential-abundance`.

It consumes raw spectra (loaded via the `data-processing` reference) and produces a
confident protein list. For DIA experiments, peptide/protein detection happens inside
the `dia-workflow` reference instead. Modified-peptide identification is covered by
`ptm-analysis`.

---

## Part 1 — Peptide Identification

### Database Search with pyOpenMS

**Goal:** Identify peptide sequences from tandem mass spectra by matching against a protein database.

**Approach:** Load a FASTA database, perform in-silico tryptic digestion to generate theoretical peptides, then match experimental spectra against theoretical fragment ion patterns to identify peptide-spectrum matches (PSMs).

```python
from pyopenms import MSExperiment, MzMLFile, FASTAFile, ProteaseDigestion
from pyopenms import ModificationsDB, AASequence

# Load FASTA database
fasta_entries = []
FASTAFile().load('uniprot_human.fasta', fasta_entries)

# In-silico digestion
digestion = ProteaseDigestion()
digestion.setEnzyme('Trypsin')
digestion.setMissedCleavages(2)

peptides = []
for entry in fasta_entries:
    seq = AASequence.fromString(entry.sequence)
    result = []
    digestion.digest(seq, result)
    peptides.extend([(entry.identifier, str(p)) for p in result])
```

For high-throughput searching, run `comet`, `MSFragger`, or `X!Tandem` from the CLI —
typical search parameters (enzyme, tolerances, fixed/variable mods) are in the usage-guide section below.

### Working with Search Results (idXML)

```python
from pyopenms import IdXMLFile, ProteinIdentification, PeptideIdentification

protein_ids = []
peptide_ids = []
IdXMLFile().load('search_results.idXML', protein_ids, peptide_ids)

for pep_id in peptide_ids:
    rt = pep_id.getRT()
    mz = pep_id.getMZ()
    for hit in pep_id.getHits():
        sequence = hit.getSequence()
        score = hit.getScore()
        charge = hit.getCharge()
```

### Spectral Library Search

```python
from pyopenms import SpectraSTSearchAlgorithm, MSExperiment

# Load spectral library
library = MSExperiment()
MzMLFile().load('spectral_library.mzML', library)

# Match query spectra against library
# Returns similarity scores and library matches
```

Building and managing the libraries themselves is covered by the `dia-workflow` reference.

### R: Search Result Processing

```r
library(MSnbase)

# Read mzIdentML results
psms <- readMzIdData('results.mzid')

# Filter to 1% FDR
psms_filtered <- psms[psms$qvalue <= 0.01, ]

# Unique peptides per protein
peptide_counts <- table(psms_filtered$accession)
```

---

## Part 2 — Target-Decoy FDR (unified)

FDR control is applied **twice** in this workflow: once at the peptide/PSM level (here)
and again at the protein-group level (Part 3). Both use the same target-decoy logic —
count decoy hits passing a score threshold as an estimate of false targets — and both
convert raw FDR into monotonic q-values. Peptide FDR and protein FDR are reported
separately; 1% at each level is standard.

### Peptide-Level FDR

```python
import numpy as np

def calculate_fdr(scores, is_decoy, score_threshold):
    above_threshold = scores >= score_threshold
    n_target = ((~is_decoy) & above_threshold).sum()
    n_decoy = (is_decoy & above_threshold).sum()
    fdr = n_decoy / n_target if n_target > 0 else 1.0
    return fdr

def find_score_at_fdr(scores, is_decoy, target_fdr=0.01):
    sorted_scores = np.sort(scores)[::-1]
    for threshold in sorted_scores:
        fdr = calculate_fdr(scores, is_decoy, threshold)
        if fdr <= target_fdr:
            return threshold
    return sorted_scores[-1]
```

The protein-level q-value computation in Part 3 reuses this same decoy-counting principle
over protein-group scores instead of PSM scores.

---

## Part 3 — Protein Inference

### The Protein Inference Problem

Peptides can map to multiple proteins (shared peptides), making protein identification ambiguous.

```python
# Example: Peptide mapping
peptide_to_proteins = {
    'PEPTIDEK': ['P12345', 'P67890'],      # Shared between paralogs
    'UNIQUER': ['P12345'],                  # Unique to P12345
    'ANOTHERONE': ['P12345'],               # Unique to P12345
    'SHAREDK': ['P67890', 'P11111'],        # Shared
}

# P12345 has 2 unique peptides -> confident identification
# P67890 has 0 unique peptides -> subset, may be grouped with P12345
```

### Parsimony Principle

**Goal:** Resolve protein identification ambiguity from shared peptides by finding the minimal protein set explaining all observed peptides.

**Approach:** Build a peptide-to-protein mapping, then greedily select proteins that cover the most unassigned peptides until all peptides are accounted for, producing a minimal explanatory protein list.

```python
def apply_parsimony(peptide_protein_map):
    '''Find minimal set of proteins explaining all peptides'''
    proteins = set()
    for prots in peptide_protein_map.values():
        proteins.update(prots)

    protein_peptides = {p: set() for p in proteins}
    for pep, prots in peptide_protein_map.items():
        for p in prots:
            protein_peptides[p].add(pep)

    covered_peptides = set()
    selected_proteins = []

    # Greedy: select protein covering most uncovered peptides
    while covered_peptides != set(peptide_protein_map.keys()):
        best_protein = max(protein_peptides.keys(),
                          key=lambda p: len(protein_peptides[p] - covered_peptides))
        new_coverage = protein_peptides[best_protein] - covered_peptides
        if not new_coverage:
            break
        selected_proteins.append(best_protein)
        covered_peptides.update(new_coverage)

    return selected_proteins
```

### Protein Groups

Group proteins that share identical peptide evidence (indistinguishable proteins).

```python
def create_protein_groups(peptide_protein_map):
    '''Group proteins with identical peptide evidence'''
    protein_peptides = {}
    for pep, prots in peptide_protein_map.items():
        for p in prots:
            protein_peptides.setdefault(p, set()).add(pep)

    # Group by peptide set
    peptide_set_to_proteins = {}
    for protein, peptides in protein_peptides.items():
        key = frozenset(peptides)
        peptide_set_to_proteins.setdefault(key, []).append(protein)

    groups = []
    for peptides, proteins in peptide_set_to_proteins.items():
        groups.append({
            'proteins': proteins,
            'peptides': list(peptides),
            'n_peptides': len(peptides),
            'is_group': len(proteins) > 1
        })

    return groups
```

### pyOpenMS Protein Inference

```python
from pyopenms import ProteinIdentification, PeptideIdentification
from pyopenms import BasicProteinInferenceAlgorithm

# Load identifications
protein_ids = []
peptide_ids = []
IdXMLFile().load('search_results.idXML', protein_ids, peptide_ids)

# Run inference
inference = BasicProteinInferenceAlgorithm()
inference.run(peptide_ids, protein_ids)

# Results include protein groups and scores
for protein_id in protein_ids:
    for hit in protein_id.getHits():
        accession = hit.getAccession()
        score = hit.getScore()
```

### R: Protein Inference

```r
library(ProteinInference)

# From peptide-protein mapping
protein_groups <- infer_proteins(
    peptides = psm_data$peptide,
    proteins = psm_data$protein,
    method = 'parsimony'
)

# Count unique peptides per group
protein_groups$n_unique <- sapply(protein_groups$peptides, function(p) {
    sum(sapply(p, function(pep) length(peptide_to_protein[[pep]]) == 1))
})
```

### Protein-Level FDR

Reuses the target-decoy principle from Part 2, applied to protein-group scores, with a
monotonic q-value pass.

```python
def protein_fdr(protein_groups, target_fdr=0.01):
    '''Calculate protein-level FDR from group scores'''
    sorted_groups = sorted(protein_groups, key=lambda x: x['score'], reverse=True)

    target_count = 0
    decoy_count = 0

    for group in sorted_groups:
        if group['is_decoy']:
            decoy_count += 1
        else:
            target_count += 1
        group['fdr'] = decoy_count / target_count if target_count > 0 else 1.0

    # Q-value
    min_fdr = 1.0
    for group in reversed(sorted_groups):
        min_fdr = min(min_fdr, group['fdr'])
        group['qvalue'] = min_fdr

    return [g for g in sorted_groups if g['qvalue'] <= target_fdr and not g['is_decoy']]
```

---

## Usage Guide

### Overview
Match MS/MS spectra to peptide sequences (database search or spectral library), control FDR, then resolve which proteins are present from the identified peptides — handling shared peptides through grouping and parsimony.

### Prerequisites
```bash
pip install pyopenms pandas numpy
# CLI search engines: MSFragger, Comet, or X!Tandem
# CLI inference: ProteinProphet (TPP), EPIFANY (OpenMS)
# R alternative: BiocManager::install(c("mzID", "MSnbase"))
```

### Quick Start
Tell your AI agent what you want to do:
- "Run a database search on my mzML files against UniProt human"
- "Set up peptide identification with trypsin digestion and 10 ppm tolerance"
- "Parse search results and filter to 1% FDR"
- "Group proteins by shared peptide evidence and apply parsimony"

### Example Prompts

#### Database Search Setup
> "Configure a database search with trypsin, 2 missed cleavages, carbamidomethyl C as fixed mod, and oxidation M as variable"

> "Set up MSFragger search with 10 ppm precursor tolerance and 0.02 Da fragment tolerance"

#### Running Searches
> "Run a peptide search against the reviewed human proteome from UniProt"

> "Perform an open modification search to identify unknown PTMs"

> "Search my DIA data against a spectral library for targeted quantification"

#### Results & FDR
> "Parse the mzIdentML output and filter to 1% peptide FDR"

> "Convert pepXML results to a pandas DataFrame with PSM scores"

> "Apply 1% protein-level FDR and report the number of protein groups"

#### Protein Inference
> "Apply parsimony principle to report minimum protein set explaining all peptides"

> "Run EPIFANY for probabilistic protein inference with FDR control"

> "Identify protein groups that share all peptide evidence (indistinguishable)"

> "Filter to proteins with at least 2 unique peptides for confident identification"

### What the Agent Will Do
1. Configure search parameters (enzyme, tolerances, modifications)
2. Set up target-decoy strategy for FDR control
3. Run database or spectral library search
4. Filter PSMs to specified peptide FDR
5. Build protein groups and apply an inference method (parsimony, probabilistic)
6. Calculate protein-level FDR and filter by unique peptides
7. Report identification statistics and a protein list with evidence summary

### Key Search Parameters

| Parameter | Typical Value | Effect |
|-----------|---------------|--------|
| Precursor tolerance | 10-20 ppm | Match window for precursor m/z |
| Fragment tolerance | 0.02-0.05 Da | Match window for fragment ions |
| Missed cleavages | 2 | Allow incomplete digestion |
| Fixed modifications | Carbamidomethyl (C) | Always present |
| Variable modifications | Oxidation (M), Phospho (STY) | May be present |

### Common Enzymes

| Enzyme | Cleavage Rule |
|--------|---------------|
| Trypsin | After K, R (not before P) |
| Lys-C | After K |
| Chymotrypsin | After F, W, Y, L |
| Asp-N | Before D |

### Inference Strategies

| Method | Description |
|--------|-------------|
| Parsimony | Minimum protein set explaining all peptides |
| Occam's Razor | Assign shared peptides to protein with most evidence |
| Probabilistic | ProteinProphet, EPIFANY - probability-based |
| All peptides | Report all possible proteins (most inclusive) |

### Key Concepts
- **Unique peptides**: Map to only one protein (strongest evidence)
- **Razor peptides**: Shared peptides assigned to the winning protein
- **Protein groups**: Proteins with indistinguishable evidence

### Tips
- Always use target-decoy FDR for quality control
- Peptide FDR and protein FDR are separate; 1% at each level is standard
- Use >= 2 unique peptides per protein for confident identification
- Variable modifications increase search space exponentially
- Report protein groups, not just lead proteins
- Be cautious with single-peptide identifications; document the inference method used
