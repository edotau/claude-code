---
name: bio-proteomics
description: End-to-end mass spectrometry proteomics — load and normalize MS data (mzML/mzXML, MaxQuant proteinGroups/evidence, DIA-NN, Spectronaut) into protein-abundance matrices; run DIA analysis library-free or library-based and build/manage spectral libraries (DIA-NN, EncyclopeDIA, EasyPQP, SpectraST, Prosit/DeepLC/MS2PIP); test differential protein abundance between conditions (limma, DEqMS, proDA, Welch's t-test, ashr shrinkage, FDR/BH); identify peptides from MS/MS spectra and resolve protein groups via parsimony and target-decoy FDR control; run proteomics QC (replicate correlation, missing-value patterns, batch effects, CV); and analyze PTMs including phosphorylation, acetylation, and ubiquitination (site localization, motif/kinase analysis, MSstatsPTM). Use for proteomics, mass spec, LFQ, TMT/iTRAQ, SILAC, phosphoproteomics, peptide-spectrum matches, and spectral libraries.
---
# Bio-Proteomics

A unified skill covering the full mass spectrometry proteomics workflow — from raw MS
data through quantification, identification/DIA analysis, QC, statistical testing, and
PTM analysis. Detailed technical content (code, parameters, version-compatibility notes)
lives in the `references/` files below. Read the reference file that matches the task.

## Pipeline at a glance

Two acquisition chains share a common first and last stage:

- **DDA chain**: data-processing → **identification** → differential-abundance
- **DIA chain**: data-processing → **dia-workflow** → differential-abundance

QC (`proteomics-qc`) runs before downstream analysis; PTM analysis (`ptm-analysis`)
branches off identification/quantification. DIA-NN report import is canonical in
`data-processing` (other references point back to it for loading results).

## Task router

| If the task is about… | Read |
|---|---|
| Loading mzML/mzXML, MaxQuant, DIA-NN, or Spectronaut output; LFQ/TMT/iTRAQ/SILAC quantification; normalization (median/quantile/VSN/LOESS); missing-value assessment | `references/data-processing.md` |
| Running DIA-NN (library-free or library-based), MSFragger-DIA, EncyclopeDIA; match-between-runs; building/merging/predicting spectral libraries (SpectraST, EasyPQP, Prosit, DeepLC, MS2PIP); iRT calibration | `references/dia-workflow.md` |
| Statistical testing for differentially abundant proteins (limma, DEqMS, proDA, Welch's t-test); design matrices/contrasts; BH correction; fold-change shrinkage (ashr); volcano plots | `references/differential-abundance.md` |
| Database search (Comet/MSFragger/X!Tandem/pyOpenMS); PSM handling; target-decoy FDR/q-values; protein inference, parsimony, protein groups, razor peptides | `references/identification.md` |
| Data quality before analysis — sample metrics, replicate correlation, missing-value patterns, batch-effect detection (PCA), CV, digestion efficiency, QC reports | `references/proteomics-qc.md` |
| PTMs — phosphorylation, acetylation, ubiquitination, methylation; mass shifts; MaxQuant Phospho(STY)Sites filtering; site localization (A-score/class I-III); motif & kinase analysis; MSstatsPTM | `references/ptm-analysis.md` |
| **pyOpenMS library API** — low-level MS data structures, feature detection, signal processing (centroiding/smoothing/baseline), file I/O, and **metabolomics** workflows | `references/pyopenms/SKILL.md` (+ `references/`) |

## Conventions across all references

- **Always log2-transform** raw intensities before normalization and statistics; replace
  zeros with NaN first to avoid `-inf`.
- **Filter** contaminants, reverse/decoy hits, and only-by-site identifications before analysis.
- **FDR**: 1% at both peptide and protein levels (DDA), and at precursor + protein level (DIA).
- **Welch's t-test** in Python: pass `equal_var=False` (scipy defaults to Student's);
  pass `method='fdr_bh'` to `statsmodels` `multipletests` (defaults to Holm-Sidak).
- **Batch effects**: include batch as a design-matrix covariate; do NOT `removeBatchEffect()`
  before testing (visualization only).
- Each reference carries its own **Version Compatibility** block — verify installed
  package versions and introspect the API on ImportError/AttributeError/TypeError rather
  than retrying blindly.

## Related skills

- `bio-database-access-uniprot-access` — protein annotations / reference FASTA
- expression-matrix / counts-ingest — analogous matrix-handling patterns
- differential-expression / deseq2-basics — analogous empirical Bayes for RNA-seq
- data-visualization / specialized-omics-plots — volcano plots, MA plots, heatmaps
