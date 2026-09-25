# Differential Abundance — Statistical Testing Between Conditions

> Source skill: `bio-proteomics-differential-abundance` (tool_type: mixed, primary_tool: limma)

## Version Compatibility

Reference examples tested with: limma 3.58+, DEqMS 1.20+, ashr 2.2+, proDA 1.20+, numpy 1.26+, pandas 2.2+, scipy 1.12+, statsmodels 0.14+

Before using code patterns, verify installed versions match. If versions differ:
- Python: `pip show <package>` then `help(module.function)` to check signatures
- R: `packageVersion('<pkg>')` then `?function_name` to verify parameters

If code throws ImportError, AttributeError, or TypeError, introspect the installed
package and adapt the example to match the actual API rather than retrying.

# Differential Protein Abundance

**"Find differentially abundant proteins between my conditions"** -> Perform statistical testing on protein intensities to identify significant abundance changes.
- R: `limma::eBayes()` for empirical Bayes moderated t-tests (preferred for small n)
- R: `DEqMS::spectraCounteBayes()` when PSM/peptide count metadata is available
- R: `proDA::test_diff()` when missing values are extensive (label-free)
- Python: `scipy.stats.ttest_ind(equal_var=False)` with `statsmodels` BH correction

## Preprocessing Pipeline

Raw mass spectrometry intensities require log2 transformation and normalization before statistical testing.

### Log2 Transformation

**Goal:** Convert right-skewed raw intensities to approximately normal distributions with stabilized variance.

**Approach:** Apply log2 to all intensity values. Replace zeros (undetected values) with NaN before transformation to avoid -inf.

```python
log2_data = np.log2(intensities.replace(0, np.nan))
```

```r
log2_matrix <- log2(intensity_matrix)
log2_matrix[!is.finite(log2_matrix)] <- NA
```

### Normalization

**Goal:** Remove systematic technical biases (sample loading, instrument drift) so that observed differences reflect biology.

**Approach:** Choose a normalization method based on data characteristics. All methods assume the majority of proteins are not differentially abundant.

**Median normalization** -- subtract per-sample median so all samples share a common center:

```python
sample_medians = log2_data.median(axis=0)
global_median = sample_medians.median()
normalized = log2_data - sample_medians + global_median
```

```r
normalized <- normalizeBetweenArrays(log2_matrix, method = 'scale')
```

| Method | When to use | R function |
|--------|-------------|------------|
| Median centering | Default for most analyses; robust to missing values | Manual or `normalizeBetweenArrays(method='scale')` |
| Cyclic loess | Unbalanced DE (more up- than down-regulated) | `normalizeBetweenArrays(method='cyclicloess')` |
| VSN | Heteroscedastic data; operates on raw intensities (skip log2) | `vsn::justvsn(raw_matrix)` |
| Quantile | TMT with complete data; avoid with many missing values | `normalizeBetweenArrays(method='quantile')` |

## Method Selection

| Scenario | Recommended | Rationale |
|----------|-------------|-----------|
| Small n (3-5 per group), protein-level data | limma | Borrows variance across proteins via empirical Bayes; adds ~10-20 effective df |
| PSM/peptide count metadata available | DEqMS | Weights variance by quantification depth per protein |
| Label-free with many missing values (>20%) | proDA | Models abundance-dependent dropout; no imputation needed |
| Large n (>10 per group), Python-only environment | Welch's t-test + BH | Variance estimates reliable at larger sample sizes |
| Complex designs (nested, multiple comparisons) | MSstats | Feature-level mixed models; handles technical replicates |

With small sample sizes (n=3-5), simple t-tests have only 4-6 degrees of freedom for variance estimation, making per-protein variances extremely noisy. Some proteins get artificially low variance (false positives), others artificially high (false negatives). limma's empirical Bayes shrinks each variance toward the global trend, dramatically improving calibration.

## limma Workflow (R)

**Goal:** Identify differentially abundant proteins using moderated statistics that borrow information across all proteins.

**Approach:** Build a linear model from the design matrix, fit contrasts, apply empirical Bayes moderation with intensity-dependent variance trend and robust fitting, then extract BH-corrected results.

```r
library(limma)

design <- model.matrix(~0 + condition, data = sample_info)
colnames(design) <- levels(factor(sample_info$condition))

fit <- lmFit(protein_matrix, design)
contrast_matrix <- makeContrasts(Treatment - Control, levels = design)
fit2 <- contrasts.fit(fit, contrast_matrix)
fit2 <- eBayes(fit2, trend = TRUE, robust = TRUE)

results <- topTable(fit2, coef = 1, number = Inf, adjust.method = 'BH')
```

`trend=TRUE` allows the prior variance to depend on mean intensity. `robust=TRUE` protects against hyper-variable outlier proteins. Results columns: `logFC`, `AveExpr`, `t`, `P.Value`, `adj.P.Val`, `B`.

For batch effects, include batch in the design matrix (do NOT use `removeBatchEffect` before testing -- that function is for visualization only):

```r
design <- model.matrix(~0 + condition + batch, data = sample_info)
```

## DEqMS Workflow (R)

**Goal:** Improve upon limma by accounting for the relationship between quantification depth and variance -- proteins quantified by more PSMs/peptides have more precise abundance estimates.

**Approach:** Run the standard limma pipeline, attach PSM/peptide counts, then apply DEqMS's count-aware empirical Bayes that fits a variance-vs-count regression.

```r
library(DEqMS)

# Standard limma pipeline through eBayes (see above), then:
fit2$count <- psm_count_per_protein[rownames(fit2$coefficients)]
fit3 <- spectraCounteBayes(fit2)

results <- outputResult(fit3, coef_col = 1)
```

Results include limma columns plus DEqMS-specific: `sca.t`, `sca.P.Value`, `sca.adj.pval`.

## proDA Workflow (R)

```r
library(proDA)

fit <- proDA(protein_matrix, design = ~condition, col_data = sample_info,
             reference_level = 'Control')
results <- test_diff(fit, conditionTreatment - conditionControl)
```

Results columns: `name`, `pval`, `adj_pval`, `diff` (log2FC), `t_statistic`, `se`.

## Python Workflow

**Goal:** Perform the full differential abundance pipeline in Python: preprocessing, statistical testing, and multiple testing correction.

**Approach:** Log2-transform and median-normalize raw intensities, run per-protein Welch's t-tests, and apply Benjamini-Hochberg correction.

```python
import numpy as np
import pandas as pd
from scipy import stats
from statsmodels.stats.multitest import multipletests

def preprocess(intensities):
    log2_data = np.log2(intensities.replace(0, np.nan))
    sample_medians = log2_data.median(axis=0)
    global_median = sample_medians.median()
    return log2_data - sample_medians + global_median

def differential_abundance(normalized, case_cols, ctrl_cols):
    results = []
    for protein in normalized.index:
        case = normalized.loc[protein, case_cols].dropna()
        ctrl = normalized.loc[protein, ctrl_cols].dropna()
        if len(case) >= 2 and len(ctrl) >= 2:
            log2fc = case.mean() - ctrl.mean()
            _, pval = stats.ttest_ind(case, ctrl, equal_var=False)
            results.append({'protein': protein, 'log2fc': log2fc, 'pvalue': pval})

    df = pd.DataFrame(results)
    df['padj'] = multipletests(df['pvalue'], method='fdr_bh')[1]
    return df
```

**Key details:**
- `equal_var=False` selects Welch's t-test (`scipy` defaults to Student's with `equal_var=True`)
- `multipletests` defaults to Holm-Sidak -- always pass `method='fdr_bh'` explicitly

## Fold Change Reporting

Raw fold changes from the linear model or t-test are the best unbiased point estimates of the true effect, but they are noisy -- proteins with no real abundance change still show small nonzero estimates from measurement noise. How to handle this depends on the downstream use case.

### When to report raw fold changes

Report the unmodified log2 fold change from the statistical test when:
- Running **GSEA or pathway analysis** that ranks all proteins by effect size (these methods rely on the full continuous distribution, including small non-significant effects)
- Performing **meta-analysis** across studies (raw FCs with standard errors are the correct input)
- The downstream consumer needs an **unbiased estimate** with associated uncertainty (report FC + SE or confidence interval)

### When to apply fold change shrinkage

Apply shrinkage when the goal is to **recover which proteins truly changed and by how much** -- i.e., effect size accuracy matters more than preserving the full distribution. ashr (R) fits a mixture prior with a point mass at zero and estimates posterior means, smoothly shrinking uncertain effects toward zero while preserving well-supported ones:

```r
library(ashr)

se <- sqrt(fit2$s2.post) * fit2$stdev.unscaled[, 1]
shrunk <- ash(fit2$coefficients[, 1], se, mixcompdist = 'normal')

shrunken_fc <- shrunk$result$PosteriorMean
lfsr <- shrunk$result$lfsr
```

ashr is preferred over hard-thresholding (zeroing FCs at a p-value cutoff) because it shrinks smoothly based on per-protein uncertainty rather than applying an arbitrary step function at padj = 0.05. Hard zeroing discards information and creates artificial discontinuities -- a protein at padj 0.049 keeps its full FC while one at 0.051 is set to zero.

In Python without ashr, there is no mature equivalent. If effect size accuracy is critical, use R with ashr. For Python-only environments, report raw fold changes with adjusted p-values and let the downstream analysis handle thresholding.

### Minimum fold change testing

To test whether fold changes exceed a biologically meaningful threshold (rather than just differ from zero), use `treat()` + `topTreat()` instead of post-hoc FC filtering with `topTable(lfc=...)`, which can inflate FDR:

```r
fit2 <- treat(fit2, lfc = log2(1.2))
results <- topTreat(fit2, coef = 1, number = Inf)
```

## Visualization

```r
library(ggplot2)

ggplot(results, aes(x = logFC, y = -log10(adj.P.Val))) +
    geom_point(aes(color = significant), alpha = 0.6) +
    geom_hline(yintercept = -log10(0.05), linetype = 'dashed') +
    geom_vline(xintercept = c(-1, 1), linetype = 'dashed') +
    scale_color_manual(values = c('grey60', 'firebrick')) +
    theme_minimal() + labs(x = 'Log2 Fold Change', y = '-Log10 Adjusted P-value')
```

## Common Pitfalls

- **Not log-transforming raw intensities** -- parametric tests assume approximately normal distributions; raw intensities are right-skewed with mean-dependent variance
- **Using Student's t-test** -- `scipy.stats.ttest_ind` defaults to `equal_var=True`; always set `equal_var=False` (Welch's) since treatment can affect both mean and variance
- **Quantile normalization with missing values** -- introduces artifacts in label-free data; use median centering or cyclic loess instead
- **`removeBatchEffect()` before testing** -- this function is for visualization only; include batch as a covariate in the design matrix for statistical testing
- **Post-hoc FC filtering via `topTable(lfc=...)`** -- can inflate FDR; use `treat()` + `topTreat()` for minimum-effect-size testing
- **Ignoring fold change uncertainty** -- raw FCs are noisy point estimates; consider ashr shrinkage when effect size accuracy matters, and always report adjusted p-values or confidence intervals alongside fold changes so downstream analyses can weight accordingly

---

## Usage Guide

### Overview
Identify proteins with significantly different abundance between experimental conditions using statistical testing, multiple testing correction, and fold change shrinkage. Covers the full pipeline from raw intensities through preprocessing, statistical modeling, and accurate effect size estimation.

### Prerequisites
```bash
pip install numpy pandas scipy statsmodels
```
```r
BiocManager::install(c("limma", "DEqMS", "ashr", "proDA"))
```

### Quick Start
Tell your AI agent what you want to do:
- "Find differentially abundant proteins between treatment and control in my intensity matrix"
- "Run limma analysis on my protein data with log2 transformation and median normalization"
- "Identify significant proteins with shrunk fold change estimates"
- "Perform differential abundance testing on my label-free proteomics data"

### Example Prompts

#### Full Pipeline
> "I have raw protein intensities in a TSV file with samples as columns. Log2 transform, median normalize, and run differential abundance testing between case and control groups. Report fold changes with shrinkage applied."

> "Analyze my TMT proteomics data for differential abundance between treatment and control. I have PSM counts per protein, so use DEqMS for the analysis."

#### Statistical Testing
> "Run limma differential analysis comparing treatment vs control groups on my normalized protein matrix"

> "Use proDA for differential testing on my label-free data that has about 30% missing values"

#### Complex Designs
> "Set up a limma model with treatment and batch as covariates in the design matrix"

> "Run paired differential analysis for my before/after samples"

#### Results and Visualization
> "Create a volcano plot of the differential abundance results"

> "Filter to proteins with adjusted p-value below 0.05 and absolute log2FC above 1"

### What the Agent Will Do
1. Load raw protein intensity matrix and sample metadata
2. Preprocess: log2 transform raw intensities, apply normalization (median centering, cyclic loess, or VSN depending on data characteristics)
3. Select statistical method based on sample size, data type, and available metadata (limma for small n, DEqMS with PSM counts, proDA for extensive missing values, Welch's t-test for large n in Python)
4. Define experimental design and contrasts
5. Fit statistical model with empirical Bayes moderation
6. Apply Benjamini-Hochberg multiple testing correction
7. Choose fold change reporting strategy based on downstream use (raw for GSEA/meta-analysis, ashr-shrunk for effect size recovery)
8. Generate results table and optional visualizations

### Statistical Methods

| Method | Best for | Key advantage |
|--------|----------|---------------|
| limma | Small n (3-5), general purpose | Borrows variance across proteins; ~10-20 extra effective df |
| DEqMS | When PSM/peptide counts available | Weights variance by quantification depth |
| proDA | Label-free with >20% missing values | Models dropout without imputation |
| Welch's t-test | Large n (>10), Python-only | Simple, reliable with sufficient samples |
| MSstats | Complex designs, technical replicates | Feature-level mixed models |

### Normalization Methods

| Method | When to use | Key assumption |
|--------|-------------|----------------|
| Median centering | Default starting point; robust to missing values | Majority of proteins unchanged |
| Cyclic loess | Unbalanced DE (asymmetric up/down regulation) | Majority of proteins unchanged |
| VSN | Heteroscedastic data; input is raw (not log2) | Parametric variance model holds |
| Quantile | TMT with complete data | Identical sample distributions |

Median normalization subtracts each sample's median log2 value and optionally re-centers to the global median, ensuring all samples share a common center. This removes systematic loading differences while preserving biological signal.

### Fold Change Reporting

Raw fold changes are noisy but unbiased estimates of the true biological effect. How to handle them depends on what comes next:

- **GSEA / pathway analysis**: Use raw fold changes for all proteins. These methods rank by effect size and rely on the full continuous distribution, including small non-significant effects. Do not zero or threshold FCs before GSEA.
- **Effect size recovery** (e.g., "which proteins truly changed and by how much?"): Apply ashr shrinkage in R to produce posterior mean estimates. ashr smoothly shrinks uncertain effects toward zero while preserving well-supported ones. This is the principled Bayesian approach.
- **Reporting tables**: Report raw FC with adjusted p-value and confidence interval. The p-value communicates uncertainty; the FC communicates magnitude. Downstream consumers can threshold as needed.
- **Cross-study comparison / meta-analysis**: Use raw FCs with standard errors as input. Shrinkage is study-specific and should not be applied before pooling.

Avoid hard-thresholding FCs at a p-value cutoff (e.g., zeroing non-significant FCs). This creates artificial discontinuities and discards information that downstream methods may need.

### Significance Thresholds

Typical thresholds for proteomics:
- **Adjusted p-value**: < 0.05 (or 0.01 for stringent)
- **Log2 fold change**: > 1 (2-fold) or > 0.58 (1.5-fold)
- Use `treat()` + `topTreat()` in limma for minimum-effect-size testing rather than post-hoc FC filtering, which can inflate FDR

### Tips
- Always log2-transform raw intensities before normalization and testing
- Always use adjusted p-values (not raw) for significance calls
- Use `equal_var=False` in `scipy.stats.ttest_ind` for Welch's t-test (the default is Student's)
- Pass `method='fdr_bh'` explicitly to `statsmodels.stats.multitest.multipletests` (the default is Holm-Sidak, not BH)
- Include batch as a covariate in the design matrix if samples were processed separately; do not use `removeBatchEffect()` before testing
- Consider ashr fold change shrinkage in R when effect size accuracy matters; for GSEA or meta-analysis, use raw FCs
- Check volcano plot symmetry. Strongly asymmetric patterns may indicate normalization issues
- Report: number tested, normalization method, statistical method, thresholds, and number significant
