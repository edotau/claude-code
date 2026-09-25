# Data Processing — Load MS Data & Normalize to Protein Abundance

> Source skill: `bio-proteomics-data-processing` (tool_type: mixed, primary_tool: pyOpenMS)

## Version Compatibility

Reference examples tested with: MSnbase 2.28+, numpy 1.26+, pandas 2.2+

Before using code patterns, verify installed versions match. If versions differ:
- Python: `pip show <package>` then `help(module.function)` to check signatures
- R: `packageVersion('<pkg>')` then `?function_name` to verify parameters

If code throws ImportError, AttributeError, or TypeError, introspect the installed
package and adapt the example to match the actual API rather than retrying.

# Proteomics Data Processing

**"Load my mass spec data and get a normalized abundance matrix"** → Parse raw mzML/mzXML or quantification-tool outputs (MaxQuant, DIA-NN, Spectronaut), filter contaminants/decoys, then summarize and normalize to protein-level abundances.
- Python: `pyopenms.MzMLFile().load()` for raw spectra, `pandas.read_csv()` for search-engine outputs
- R: `MSnbase::readMSData()` for raw, `MSstats::dataProcess()` for feature-to-protein summarization
- Python/R: MaxLFQ-style median normalization, TMT/iTRAQ reporter extraction, SILAC ratios

## Pipeline Context

This is the **first stage** of both proteomics chains:
- **DDA chain**: data-processing → `identification` → `differential-abundance`
- **DIA chain**: data-processing → `dia-workflow` → `differential-abundance`

Quality assessment lives in the separate `proteomics-qc` reference; modified-peptide
quantification lives in `ptm-analysis`. Reference those rather than duplicating them.

---

## Part 1 — Data Import

### Loading mzML/mzXML Files with pyOpenMS

**Goal:** Parse raw mass spectrometry data files into memory for programmatic access.

**Approach:** Load mzML/mzXML into an MSExperiment object, then iterate spectra by MS level to access peaks and precursor info.

```python
from pyopenms import MSExperiment, MzMLFile, MzXMLFile

exp = MSExperiment()
MzMLFile().load('sample.mzML', exp)

for spectrum in exp:
    if spectrum.getMSLevel() == 1:
        mz, intensity = spectrum.get_peaks()
    elif spectrum.getMSLevel() == 2:
        precursor = spectrum.getPrecursors()[0]
        precursor_mz = precursor.getMZ()
```

### Loading MaxQuant Output

**Goal:** Import MaxQuant proteinGroups.txt with contaminant and decoy filtering.

**Approach:** Read the TSV file, remove reverse hits, contaminants, and site-only identifications, then extract intensity columns.

```python
import pandas as pd

protein_groups = pd.read_csv('proteinGroups.txt', sep='\t', low_memory=False)

# Filter contaminants and reverse hits
contam_col = 'Potential contaminant' if 'Potential contaminant' in protein_groups.columns else 'Contaminant'
protein_groups = protein_groups[
    (protein_groups.get(contam_col, '') != '+') &
    (protein_groups.get('Reverse', '') != '+') &
    (protein_groups.get('Only identified by site', '') != '+')
]

# Extract intensity columns (LFQ or iBAQ)
intensity_cols = [c for c in protein_groups.columns if c.startswith('LFQ intensity') or c.startswith('iBAQ ')]
if not intensity_cols:
    intensity_cols = [c for c in protein_groups.columns if c.startswith('Intensity ') and 'Intensity L' not in c]
intensities = protein_groups[['Protein IDs', 'Gene names'] + intensity_cols]
```

### Loading DIA-NN / Spectronaut Output (canonical import)

**Goal:** Import DIA-NN long-format report and reshape into a protein-by-sample quantification matrix.

**Approach:** Pivot the report table on protein group and run columns, using MaxLFQ values. This is the canonical DIA-NN report import used across the suite — the `dia-workflow` reference produces these files and refers back here for loading them.

```python
diann_report = pd.read_csv('report.tsv', sep='\t')

# Pivot to protein-level matrix
protein_matrix = diann_report.pivot_table(
    index='Protein.Group', columns='Run', values='PG.MaxLFQ', aggfunc='first'
)
```

DIA-NN also emits wide-format matrices (`report.pg_matrix.tsv`) directly; see the
usage-guide section below for loading those and for the Spectronaut long-to-wide pivot.

### R: Loading with MSnbase

**Goal:** Load raw MS data in R for interactive exploration of spectra and metadata.

**Approach:** Use MSnbase's on-disk reading mode to access spectra and feature metadata without loading all data into memory.

```r
library(MSnbase)

raw_data <- readMSData('sample.mzML', mode = 'onDisk')
spectra <- spectra(raw_data)
header_info <- fData(raw_data)
```

### Missing Value Assessment

**Goal:** Quantify missing value patterns across proteins and samples in an intensity matrix.

**Approach:** Count NaN values per protein and per sample, then compute overall missing percentage.

```python
def assess_missing_values(df, intensity_cols):
    missing_per_protein = df[intensity_cols].isna().sum(axis=1)
    missing_per_sample = df[intensity_cols].isna().sum(axis=0)

    total_missing = df[intensity_cols].isna().sum().sum()
    total_values = df[intensity_cols].size
    missing_pct = 100 * total_missing / total_values

    return {'per_protein': missing_per_protein, 'per_sample': missing_per_sample, 'total_pct': missing_pct}
```

Missing-value *patterns* (MCAR/MAR/MNAR) determine downstream imputation strategy — see the usage-guide section below.

---

## Part 2 — Quantification & Normalization

### Label-Free Quantification (LFQ)

**Intensity-Based (MaxLFQ algorithm)** — log2-transform, then median-center per sample against the global median.

```python
import pandas as pd
import numpy as np

def maxlfq_normalize(intensities):
    '''Simplified MaxLFQ normalization'''
    log_int = np.log2(intensities.replace(0, np.nan))

    # Median centering per sample
    sample_medians = log_int.median(axis=0)
    global_median = sample_medians.median()
    normalized = log_int - sample_medians + global_median

    return normalized
```

**Spectral Counting (NSAF)** — normalized spectral abundance factor.

```python
def spectral_count_normalize(counts, total_spectra):
    '''Normalized spectral abundance factor (NSAF)'''
    # Divide by protein length, then by total
    nsaf = counts / total_spectra
    return nsaf / nsaf.sum()
```

### TMT/iTRAQ Quantification

**R (MSnbase):** load reporter ions, normalize against a reference channel, summarize to protein level.

```r
library(MSnbase)

# Load reporter ion data
tmt_data <- readMSnSet('tmt_data.txt')

# Normalize with reference channel
tmt_normalized <- normalize(tmt_data, method = 'center.median')

# Summarize to protein level
protein_data <- combineFeatures(tmt_normalized, groupBy = fData(tmt_data)$protein,
                                 fun = 'median')
```

**Python:** extract reporter ion intensities from MS2 spectra by m/z window (10-plex defaults below; isotope-impurity correction in the usage-guide section).

```python
def extract_tmt_intensities(spectrum, reporter_mz, tolerance=0.003):
    '''Extract TMT reporter ion intensities'''
    mz, intensity = spectrum.get_peaks()
    tmt_intensities = {}

    for channel, target_mz in reporter_mz.items():
        mask = np.abs(mz - target_mz) < tolerance
        if mask.any():
            tmt_intensities[channel] = intensity[mask].max()
        else:
            tmt_intensities[channel] = 0

    return tmt_intensities

TMT_10PLEX = {'126': 126.127726, '127N': 127.124761, '127C': 127.131081,
              '128N': 128.128116, '128C': 128.134436, '129N': 129.131471,
              '129C': 129.137790, '130N': 130.134825, '130C': 130.141145,
              '131': 131.138180}
```

### SILAC Quantification

**Goal:** Compute heavy/light ratios from metabolic labeling.

```python
def calculate_silac_ratio(heavy_intensity, light_intensity):
    '''Calculate SILAC H/L ratio'''
    if light_intensity > 0 and heavy_intensity > 0:
        return np.log2(heavy_intensity / light_intensity)
    return np.nan

# Typical mass shifts
SILAC_SHIFTS = {
    'Arg10': 10.008269,  # 13C6 15N4 Arginine
    'Lys8': 8.014199,    # 13C6 15N2 Lysine
    'Arg6': 6.020129,    # 13C6 Arginine
    'Lys6': 6.020129     # 13C6 Lysine
}
```

### MSstats Workflow (R)

**Goal:** Convert MaxQuant output into normalized protein-level abundance estimates using MSstats feature-to-protein summarization.

**Approach:** Reformat MaxQuant evidence and proteinGroups files into MSstats input format, then apply median equalization normalization with Tukey's median polish for protein-level summarization.

```r
library(MSstats)

# Prepare input from MaxQuant
maxquant_input <- MaxQtoMSstatsFormat(
    evidence = read.table('evidence.txt', sep = '\t', header = TRUE),
    proteinGroups = read.table('proteinGroups.txt', sep = '\t', header = TRUE),
    annotation = read.csv('annotation.csv')
)

# Process and normalize
processed <- dataProcess(maxquant_input, normalization = 'equalizeMedians',
                         summaryMethod = 'TMP', censoredInt = 'NA')

# Protein-level summary
protein_summary <- quantification(processed)
```

---

## Usage Guide

### Overview
Load mass spectrometry data from raw and tool-output formats (mzML, MaxQuant, DIA-NN, Spectronaut), then convert the signals into normalized protein-abundance matrices using label-free, isobaric, or metabolic labeling strategies.

### Prerequisites
```bash
pip install pyopenms pandas numpy scipy
# R packages: BiocManager::install(c("MSnbase", "MSstats", "DEP"))
```

### Quick Start
Tell your AI agent what you want to do:
- "Load my MaxQuant proteinGroups.txt file and filter contaminants"
- "Read mzML files from my experiment folder"
- "Import DIA-NN output and prepare for statistical analysis"
- "Normalize my MaxLFQ intensities using median centering"
- "Process TMT reporter ion intensities from my experiment"

### Example Prompts

#### Loading Search Engine Output
> "Load MaxQuant proteinGroups.txt, remove contaminants and reverse hits, and log2-transform the LFQ intensities"

> "Import the DIA-NN report.tsv and create a protein abundance matrix"

#### Loading Raw MS Data
> "Read all mzML files in my data folder using pyOpenMS"

> "Parse the MS1 spectra from my mzML file and extract precursor information"

#### Normalization
> "Apply median centering normalization to my protein intensity matrix"

> "Use quantile normalization to correct for batch effects between runs"

> "Normalize my TMT data using the internal reference channel"

#### Label-Free & Labeled Quantification
> "Calculate MaxLFQ intensities from peptide-level data"

> "Summarize peptide intensities to protein level using top3 method"

> "Extract TMT reporter ion intensities and correct for isotope impurity"

> "Calculate SILAC H/L ratios from my heavy/light pairs"

#### Missing Value Handling
> "Check the missing value pattern in my MaxQuant output"

> "Impute missing values using KNN for MAR pattern and MinProb for MNAR"

> "Filter to proteins with at least 2 unique peptides and valid values in 70% of samples"

### What the Agent Will Do
1. Load data from specified format (mzML, MaxQuant, DIA-NN, Spectronaut)
2. Apply standard filtering (contaminants, decoys, only-by-site)
3. Log2-transform intensities
4. Apply appropriate normalization method
5. Identify missing value pattern (MCAR/MAR/MNAR) and impute if needed
6. Summarize to protein level and report QC metrics (CV, correlation, missing %)

### Supported Formats

| Format | Description | Tool |
|--------|-------------|------|
| mzML | Open standard for MS data | pyOpenMS, MSnbase |
| mzXML | Legacy open format | pyOpenMS |
| proteinGroups.txt | MaxQuant protein output | pandas |
| evidence.txt | MaxQuant peptide output | pandas |
| report.tsv | DIA-NN long-format output | pandas |
| report.pg_matrix.tsv | DIA-NN wide protein matrix | pandas |
| spectronaut_report.tsv | Spectronaut export | pandas (long→wide pivot) |

### Loading DIA-NN Wide Matrices
DIA-NN emits both a long-format `report.tsv` (pivot on `Protein.Group` × `Run`, value `PG.MaxLFQ`)
and ready-made wide matrices. Load the wide protein matrix directly:

```python
import pandas as pd
import numpy as np

proteins = pd.read_csv('report.pg_matrix.tsv', sep='\t').set_index('Protein.Group')
log2_proteins = np.log2(proteins.replace(0, np.nan))  # DIA-NN outputs raw intensities
```

Spectronaut long-format exports pivot on `PG.ProteinGroups` × `R.FileName`, value `PG.Quantity`.

### Normalization Methods

| Method | Description | Use Case |
|--------|-------------|----------|
| Median centering | Shift to common median | General purpose |
| Quantile | Force identical distributions | Strong batch effects |
| LOESS | Local regression | Non-linear effects |
| VSN | Variance stabilization | Heteroscedastic data |

### Missing Value Patterns & Handling

| Type | Meaning | Method |
|------|---------|--------|
| MCAR | Missing completely at random (rare) | Mean/median imputation |
| MAR | Missing at random | KNN imputation |
| MNAR | Missing not at random (low abundance) | MinDet, MinProb, left-censored |

### Tips
- Use `low_memory=False` when loading large MaxQuant files
- Always log2-transform intensities before normalization and statistics
- Filter contaminants, reverse, and only-by-site columns before analysis
- Check CV across replicates (technical <20%, biological <40%)
- Use PCA to verify normalization removed batch effects
- Document the imputation method - it affects downstream statistics
- Check for batch effects in missing value patterns
