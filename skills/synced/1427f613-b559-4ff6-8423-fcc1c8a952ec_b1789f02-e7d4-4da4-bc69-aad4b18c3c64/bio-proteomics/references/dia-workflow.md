# DIA Workflow — DIA Analysis & Spectral Libraries

> Source skill: `bio-proteomics-dia-workflow` (tool_type: mixed, primary_tool: diann)

## Version Compatibility

Reference examples tested with: numpy 1.26+, pandas 2.2+, matplotlib 3.8+

Before using code patterns, verify installed versions match. If versions differ:
- Python: `pip show <package>` then `help(module.function)` to check signatures
- R: `packageVersion('<pkg>')` then `?function_name` to verify parameters
- CLI: `<tool> --version` then `<tool> --help` to confirm flags

If code throws ImportError, AttributeError, or TypeError, introspect the installed
package and adapt the example to match the actual API rather than retrying.

# DIA Workflow

**"Analyze my DIA proteomics data"** → Process data-independent acquisition MS data to identify and quantify proteins using library-free or library-based workflows, and build/manage the spectral libraries those workflows consume.
- CLI: `diann` for end-to-end DIA analysis with neural network scoring
- CLI: `EncyclopeDIA` for chromatogram library-based quantification
- CLI: `spectrast`, `easypqp` for empirical library building
- Python: `Prosit`/`DeepLC`/`MS2PIP` for deep learning-predicted libraries

## Pipeline Context

This is the **middle stage** of the DIA chain:
`data-processing` → **dia-workflow** → `differential-abundance`.

It is the DIA-equivalent of the DDA `identification` reference. Loading and normalizing the
resulting matrices is handled by `data-processing`; statistical testing is handled by
`differential-abundance`.

> **DIA-NN report import is canonical in `data-processing`.** The pivot/load patterns for
> `report.tsv`, `report.pg_matrix.tsv`, and Spectronaut exports live there — refer to that
> reference for loading results rather than re-implementing it here.

---

## Part 1 — DIA Analysis

### DIA-NN Library-Free Analysis

**Goal:** Run DIA proteomics analysis without a pre-built spectral library, generating one from the data itself.

**Approach:** Use DIA-NN in library-free mode with FASTA-based in silico digestion and deep learning prediction.

```bash
# Library-free mode (generates library from data)
diann \
    --f sample1.mzML \
    --f sample2.mzML \
    --lib "" \
    --threads 8 \
    --verbose 1 \
    --out report.tsv \
    --qvalue 0.01 \
    --matrices \
    --out-lib generated_lib.tsv \
    --gen-spec-lib \
    --predictor \
    --fasta uniprot_human.fasta \
    --fasta-search \
    --min-fr-mz 200 \
    --max-fr-mz 1800 \
    --met-excision \
    --cut K*,R* \
    --missed-cleavages 1 \
    --min-pep-len 7 \
    --max-pep-len 30 \
    --min-pr-mz 300 \
    --max-pr-mz 1800 \
    --min-pr-charge 1 \
    --max-pr-charge 4 \
    --unimod4 \
    --var-mods 1 \
    --var-mod UniMod:35,15.994915,M \
    --reanalyse \
    --smart-profiling
```

### DIA-NN with Spectral Library

**Goal:** Analyze DIA data using a pre-built or predicted spectral library for targeted extraction.

**Approach:** Supply an existing spectral library to DIA-NN for guided peptide detection and quantification.

```bash
# Use pre-built or predicted library
diann \
    --f sample1.mzML \
    --f sample2.mzML \
    --lib spectral_library.tsv \
    --threads 8 \
    --verbose 1 \
    --out report.tsv \
    --qvalue 0.01 \
    --matrices \
    --reanalyse \
    --smart-profiling
```

### DIA-NN Output Files

```
report.tsv                    # Main quantification report (long format)
report.stats.tsv              # Run statistics
report.pg_matrix.tsv          # Protein group quantities (wide format)
report.pr.matrix.tsv          # Precursor quantities (wide format)
report.gg_matrix.tsv          # Gene group quantities (wide format)
generated_lib.tsv             # Generated spectral library (if requested)
```

Load these into Python/R via the `data-processing` reference (canonical DIA-NN import).

### Match Between Runs

**Goal:** Transfer peptide identifications between runs to reduce missing values.

**Approach:** Enable DIA-NN's two-pass reanalysis with the `--reanalyse` flag for automatic match-between-runs.

```bash
# DIA-NN MBR is automatic with --reanalyse flag
# First pass: identifies peptides per run
# Second pass: transfers IDs between runs

diann \
    --f *.mzML \
    --lib library.tsv \
    --reanalyse \
    --out report_mbr.tsv
```

### MSFragger-DIA Analysis

**Goal:** Perform DIA analysis using MSFragger as an alternative to DIA-NN.

**Approach:** Generate a predicted spectral library with EasyPQP from search results, then convert to the desired format (see Part 2).

```bash
# MSFragger for DIA (alternative to DIA-NN)
# Requires FragPipe GUI or command-line workflow

# Generate predicted library with EasyPQP
easypqp library \
    --in psm_results.tsv \
    --out library.pqp \
    --psmtsv \
    --rt_reference irt.tsv

# Convert to DIA-NN format
easypqp convert \
    --in library.pqp \
    --out library.tsv \
    --format diann
```

### Spectronaut Export Processing

Spectronaut produces a long-format report; pivot it to a protein matrix (`PG.ProteinGroups`
× `R.FileName`, value `PG.Quantity`). The reshape pattern is documented in the
`data-processing` reference alongside the DIA-NN import.

### DIA Quality Metrics

**Goal:** Assess DIA data quality by summarizing identification counts and missing value rates per run.

```r
library(tidyverse)

report <- read_tsv('report.tsv')

# Identifications per run
ids_per_run <- report %>%
    group_by(Run) %>%
    summarise(
        precursors = n_distinct(Precursor.Id),
        proteins = n_distinct(Protein.Group),
        genes = n_distinct(Genes)
    )

# Missing value analysis
proteins <- read_tsv('report.pg_matrix.tsv')
protein_values <- proteins %>% select(-Protein.Group)
missing_pct <- colSums(protein_values == 0 | is.na(protein_values)) / nrow(protein_values) * 100
```

Deeper QC (CV, correlation, batch effects) belongs to the `proteomics-qc` reference.

### DIA vs DDA Comparison

| Feature | DIA | DDA |
|---------|-----|-----|
| Acquisition | All precursors fragmented | Top-N precursors selected |
| Missing values | Lower (5-20%) | Higher (30-50%) |
| Dynamic range | Better for low-abundance | Better for high-abundance |
| Library required | Optional (library-free) | Not applicable |
| Quantification | More reproducible | More variable |
| Analysis tools | DIA-NN, Spectronaut | MaxQuant, MSFragger |

---

## Part 2 — Spectral Libraries

### Build Library from DDA Data

**SpectraST (TPP)** — consensus library from search results.

```bash
# Build library from search results
spectrast -cNlibrary.splib -cAC search_results.pep.xml

# Filter library for quality
spectrast -cNfiltered.splib -cAQ library.splib

# Convert to other formats
spectrast -cNlibrary.tsv -cM library.splib
```

**EasyPQP (Skyline/OpenMS)**

```bash
# Build library from search results
easypqp library \
    --in psm_results.tsv \
    --out library.pqp \
    --psmtsv \
    --rt_reference irt.tsv

# Convert to TSV format
easypqp convert \
    --in library.pqp \
    --out library.tsv \
    --format openswath
```

**EncyclopeDIA (Walnut)** — chromatogram library from DIA.

```bash
# Build chromatogram library from DIA
EncyclopeDIA \
    -i sample1.mzML \
    -i sample2.mzML \
    -l wide_window_library.dlib \
    -f uniprot.fasta \
    -o results

# Search with narrow-window DIA
EncyclopeDIA \
    -i narrow_sample.mzML \
    -l narrow_library.elib \
    -f uniprot.fasta \
    -o search_results
```

### Predicted Libraries

**Prosit (deep learning fragmentation):** submit peptides + charge + collision energy to
the Prosit API and parse predictions into library format.

```python
import requests
import pandas as pd

peptides = pd.DataFrame({
    'modified_sequence': ['PEPTIDEK', 'ANOTHERPEPTIDER'],
    'collision_energy': [30, 30],
    'precursor_charge': [2, 2]
})

response = requests.post(
    'https://www.proteomicsdb.org/prosit/api/predict',
    json=peptides.to_dict(orient='records')
)
predictions = response.json()
```

**DeepLC (retention-time prediction)** and **MS2PIP (fragmentation prediction)** round out
the predicted-library toolkit — full calibrate/predict snippets are in the usage-guide section below.

### Library Formats

DIA-NN TSV (required columns):

```
PrecursorMz    ProductMz    Annotation    ProteinId    GeneName
PeptideSequence    ModifiedSequence    PrecursorCharge
FragmentCharge    FragmentType    FragmentSeriesNumber
NormalizedRetentionTime    LibraryIntensity
```

OpenSWATH and Spectronaut have their own column schemas; conversion helpers and the full
column lists are in the usage-guide section below.

### Library QC

```python
import pandas as pd

library = pd.read_csv('library.tsv', sep='\t')

print(f"Precursors: {library['ModifiedSequence'].nunique()}")
print(f"Proteins: {library['ProteinId'].nunique()}")
print(f"Transitions per precursor: {len(library) / library['ModifiedSequence'].nunique():.1f}")
```

RT and charge-state distribution plots are in the usage-guide section below.

### Merge Libraries

**Goal:** Combine multiple spectral libraries into a single non-redundant library, keeping the highest-quality spectra for each precursor.

**Approach:** Concatenate library tables, rank precursors by total fragment intensity, and deduplicate by keeping the best-scoring entry per precursor-fragment combination.

```python
import pandas as pd

lib1 = pd.read_csv('library1.tsv', sep='\t')
lib2 = pd.read_csv('library2.tsv', sep='\t')

combined = pd.concat([lib1, lib2])

# Keep entry with highest total intensity per precursor
precursor_intensity = combined.groupby('ModifiedSequence')['LibraryIntensity'].sum()
combined['total_int'] = combined['ModifiedSequence'].map(precursor_intensity)
combined = combined.sort_values('total_int', ascending=False)
combined = combined.drop_duplicates(subset=['ModifiedSequence', 'FragmentType', 'FragmentSeriesNumber'])
combined = combined.drop('total_int', axis=1)

combined.to_csv('merged_library.tsv', sep='\t', index=False)
```

### iRT Calibration

```python
# Biognosys iRT peptides for retention time calibration
IRT_PEPTIDES = {
    'LGGNEQVTR': -24.92,
    'GAGSSEPVTGLDAK': 0.00,  # Reference
    'VEATFGVDESNAK': 12.39,
    'YILAGVENSK': 19.79,
    'TPVISGGPYEYR': 28.71,
    'TPVITGAPYEYR': 33.38,
    'DGLDAASYYAPVR': 42.26,
    'ADVTPADFSEWSK': 54.62,
    'GTFIIDPGGVIR': 70.52,
    'GTFIIDPAAVIR': 87.23,
    'LFLQFGAQGSPFLK': 100.00
}

def irt_to_nrt(irt, gradient_length=60):
    '''Convert iRT to normalized RT (0-1 scale)'''
    return (irt + 24.92) / 124.92  # Scale to 0-1
```

---

## Usage Guide

### Overview
Process data-independent acquisition (DIA) mass spectrometry data for comprehensive proteome quantification with fewer missing values than DDA, and build/manage the spectral libraries those workflows consume (empirical, predicted, or hybrid).

### Prerequisites
```bash
pip install pandas numpy matplotlib
# CLI: DIA-NN (recommended), MSFragger-DIA, OpenSWATH, EncyclopeDIA, EasyPQP, SpectraST
# Deep learning: Prosit (web), MS2PIP, DeepLC
# Commercial: Spectronaut
```

### Quick Start
Tell your AI agent what you want to do:
- "Run DIA-NN on my mzML files in library-free mode"
- "Process DIA data using a spectral library I built"
- "Build a spectral library from my DDA search results"
- "Generate a predicted library using Prosit for my protein list"

### Example Prompts

#### Library-Free Analysis
> "Run DIA-NN in library-free mode against the UniProt human FASTA with 1% FDR"

> "Process my DIA mzML files without a spectral library using deep learning prediction"

#### Library-Based Analysis
> "Search my DIA data against the spectral library from my previous DDA experiments"

> "Run DIA-NN with my Prosit-predicted library for targeted analysis"

> "Use match-between-runs (--reanalyse) with my spectral library for improved coverage"

#### Building Empirical Libraries
> "Create a spectral library from my MaxQuant msms.txt using EasyPQP"

> "Build a SpectraST library from my pepXML search results"

> "Combine libraries from multiple DDA experiments into a consensus library"

#### Generating Predicted Libraries
> "Use Prosit to predict spectra for the human proteome"

> "Generate a predicted library with MS2PIP for my target protein list"

> "Create a DeepLC retention time library for my FASTA sequences"

#### Format Conversion & QC
> "Convert my SpectraST .splib to DIA-NN .tsv format"

> "Check the number of precursors and transitions in my library"

> "Analyze the retention time coverage and charge state distribution"

### What the Agent Will Do
1. Configure DIA-NN parameters (FASTA, library, tolerances) or library-build inputs
2. Run search in library-free or library-based mode, or build/predict a library
3. Apply FDR filtering at precursor and protein level
4. Export protein/precursor matrices (loaded via the data-processing reference)
5. Filter libraries for quality and convert to the target format

### Library-Free vs Library-Based

| Mode | Description | Use When |
|------|-------------|----------|
| Library-free | Deep learning predicts spectra | Quick analysis, no prior data |
| Library-based | Match to experimental spectra | Higher sensitivity for known targets |
| Hybrid | Predicted + empirical library | Best coverage for large studies |

### Key DIA-NN Parameters
- `--qvalue 0.01` - 1% FDR at precursor and protein level
- `--reanalyse` - Two-pass analysis for match-between-runs
- `--smart-profiling` - Improved quantification accuracy
- `--min-fr-mz 200 --max-fr-mz 1800` - Fragment ion range

### Library Types

| Type | Source | Pros | Cons |
|------|--------|------|------|
| Empirical | DDA experiments | Highest quality spectra | Limited coverage |
| Predicted | Deep learning (Prosit) | Complete proteome | Slightly lower accuracy |
| Hybrid | Both combined | Best coverage + quality | More complex to build |

### Key Library Formats

| Format | Tool | Extension |
|--------|------|-----------|
| SpectraST | TPP | .splib |
| PQP | OpenMS/Skyline | .pqp |
| DLIB | EncyclopeDIA | .dlib |
| TSV | DIA-NN/OpenSWATH | .tsv |
| SSL | Spectronaut | .kit |

### DeepLC Retention Time Prediction
```python
from deeplc import DeepLC

dlc = DeepLC()
calibration_peptides = ['GAGSSEPVTGLDAK', 'VEATFGVDESNAK']
calibration_rts = [22.4, 33.1]

dlc.calibrate_preds(
    seq_df=pd.DataFrame({'seq': calibration_peptides, 'rt': calibration_rts})
)
predicted_rts = dlc.make_preds(seq_df=pd.DataFrame({'seq': ['PEPTIDEK', 'ANOTHERPEPTIDER']}))
```

### MS2PIP Fragmentation Prediction
```python
from ms2pip import Predictor

predictor = Predictor(model='HCD2021')
peptide_df = pd.DataFrame({
    'peptide': ['PEPTIDEK', 'ANOTHERPEPTIDER'],
    'charge': [2, 2],
    'modifications': ['', '']
})
predictions = predictor.predict(peptide_df)
```

### OpenSWATH Library Conversion
```python
import pandas as pd

library = pd.DataFrame({
    'PrecursorMz': precursor_mz, 'ProductMz': product_mz,
    'LibraryIntensity': intensity, 'NormalizedRetentionTime': rt,
    'PrecursorCharge': charge, 'ProductCharge': 1,
    'FragmentType': ion_type,  # 'b' or 'y'
    'FragmentSeriesNumber': ion_num,
    'ModifiedPeptideSequence': mod_seq, 'PeptideSequence': sequence,
    'ProteinId': protein, 'GeneName': gene, 'Decoy': 0
})
library.to_csv('library_openswath.tsv', sep='\t', index=False)
```

Spectronaut library key columns: `ModifiedPeptide`, `StrippedPeptide`, `PrecursorCharge`,
`PrecursorMz`, `iRT`, `FragmentLossType`, `FragmentCharge`, `FragmentType`, `FragmentNumber`,
`RelativeIntensity`, `FragmentMz`, `ProteinGroups`, `Genes`, `ProteinIds`.

### Library QC Plots
```python
import matplotlib.pyplot as plt

rts = library.groupby('ModifiedSequence')['NormalizedRetentionTime'].first()
plt.hist(rts, bins=50)
plt.xlabel('Normalized RT'); plt.ylabel('Precursors')
plt.savefig('rt_distribution.png')

charges = library.groupby('ModifiedSequence')['PrecursorCharge'].first()
print(charges.value_counts())
```

### Tips
- Library-free mode is often sufficient for discovery proteomics
- Use --reanalyse for better quantification across many samples
- DIA typically has fewer missing values than DDA
- Filter to 1% FDR at both precursor and protein levels
- Empirical libraries from the same sample type give best results; predicted libraries enable analysis without prior DDA data
- Aim for 6-10 transitions per precursor; check RT coverage spans your gradient
