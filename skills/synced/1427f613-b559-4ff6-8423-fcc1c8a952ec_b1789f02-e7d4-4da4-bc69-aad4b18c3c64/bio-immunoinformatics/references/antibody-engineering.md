# Antibody Engineering — CDR Annotation, Developability Liabilities, ESM Substitution, RAPID Triage

The therapeutic-antibody de-risking workflow used across the BI repos
`digitalbtd-insilico-suite` (immunoinformatics platform) and
`digitalbtd-portal/src/automated_r0` (the **RAPID** R0-triage pipeline). This is the
antibody-sequence-engineering layer that feeds MHC-II immunogenicity scoring
(`references/mhc-binding-prediction.md`). Read this when annotating CDRs, scanning for
developability liabilities, proposing substitutions, or running/understanding RAPID.

> **Version note:** examples reflect the repos as of 2026-06. `ig-align` is a private
> Nexus package (`ig-align==0.4rc5`); ESM is `fair-esm==2.0.0` + `transformers==4.53.3`;
> `biopython>=1.84`, `pandas>=2.2`, `numpy>=1.23`. On ImportError/AttributeError,
> introspect the installed package rather than retrying — APIs drift across rc builds.

---

## 1. CDR annotation & numbering (ig-align / AbRSA)

`anarci` and `abnumber` are **not used** here — they were removed in favor of `ig-align`
(AbRSA-based, bundled rpsblast + CDD; no hmmscan dependency).

```python
from ig_align.fv_align.abrsa import AbRsa          # CDR/framework annotation
from ig_align.fv_align.germline import GermlineCloseness  # nearest-germline % + family
from ig_align.search.domains import IgCdSearch      # Ig domain detection (rpsblast)
from ig_align.search.igblast import Species         # IgBLAST species config
```

**One scheme for the whole Fv today — abm — but the scheme is a seam, not a constant.**
`CHAIN_CDR_SCHEME` (`src/bioinformatics/constants.py:60`) is abm for every chain type:

| Chain | Scheme | Constant |
|---|---|---|
| Heavy (H) | `abm` | `CHAIN_CDR_SCHEME["H"] = "abm"` |
| Kappa (K) | `abm` | `CHAIN_CDR_SCHEME["K"] = "abm"` |
| Lambda (L) | `abm` | `CHAIN_CDR_SCHEME["L"] = "abm"` |

That collapse is scientifically free on the light chain: **Kabat, Chothia and AbM define
identical LCDR1/2/3**, so the earlier `K/L = kabat` mapping produced byte-identical
boundaries (measured on brolucizumab-L: `QASEIIHSWLA` / `LASTLAS` / `QNVYLASTNGAN` under all
three). Within that family the schemes diverge only in the **heavy** chain; IMGT is the
outlier that renumbers both (it shortens LCDR1 to `EIIHSW` and LCDR2 to `LAS`).
Measured on theralizumab-H:

| | abm | kabat | chothia | imgt |
|---|---|---|---|---|
| HCDR1 | `GYTFTSYYIH` | `SYYIH` | `GYTFTSY` | `GYTFTSYY` |
| HCDR2 | `CIYPGNVNTNYNEKFKD` | `CIYPGNVNTNYNEKFKD` | `YPGNVN` | `IYPGNVNT` |
| HCDR3 | `SHYGLDWNFDV` | `SHYGLDWNFDV` | `SHYGLDWNFDV` | `TRSHYGLDWNFDV` |

So HC scheme choice is not cosmetic: RAPID's GDB records carry mixed conventions
(HC≈IMGT, LC≈Kabat), and abm vs IMGT shifts HCDR3 by 2 N-terminal residues and HCDR1 by 2.
CDR boundaries steer region-segmented tiling — CDRs always step by 1, framework by `fr_step`
(`src/pathogen_mimicry/workflows.py:324`) — so the scheme changes which peptides reach the
mimicry/MHC-II search. Prefer the GDB feature annotation when present
(`internal_type == "CDR"`), fall back to AbRSA runtime annotation on the V-region.
`CDR_LOCATION_WEIGHTS` would amplify this in liability scoring, but that table is reference
data only — defined at `src/bioinformatics/motifs.py:1257` with no code reading it.

Supported schemes are `ABRSA_SUPPORTED_SCHEMES = {"abm", "kabat", "chothia", "imgt"}` with
`ABRSA_DEFAULT_SCHEME = "abm"` (`src/bioinformatics/constants.py:56-57`).

```python
# insilico-suite pattern: returns {"CDR1": (start,end), "CDR2": ..., "CDR3": ...}
from bioinformatics.align import AbRegionr
regions = AbRegionr("abm").cdr_regions(vh_seq)          # AbRegionr(None) resolves per chain
```

**CDR-resolution order in RAPID** (most → least trusted): (1) GDB feature annotations →
(2) AbRSA runtime annotation → (3) chain-type inference from locus suffix / keyword regex.

---

## 2. Developability liability scanning

Sequence-liability model from **BI Computational Biochemistry & Bioinformatics**
(`automated_r0/constants.py`). Score per hit:

```
score = LIABILITY_TYPE_WEIGHTS[type] × MOTIF_PROPENSITY[type][motif]
        × (CDR_LOCATION_WEIGHTS[cdr] if hit is CDR-resident else 1)
```

**Liability type weights** (severity class):

| Type | Weight | Motif(s) → propensity |
|---|---|---|
| N_glycosylation | **400** | `N[^P][ST]` (sequon) — expanded to 38 literal triplets `NXS/NXT` for scanning |
| Unpaired_Cys | **400** | `C` (track-only) |
| Deamidation | 2 | NG·8, NS·8, NH·4, NN·4, NT·4, NA·2, NY·2, NF·2, NQ·2 |
| Isomerization | 2 | DG·8, DS·8, DD·4, DY·4, DN·2, DR·2, DT·1, DH·1 |
| Oxidation | 1 | M·2, W·2 |
| Fragmentation | 1 | DP·1, DY·1 |
| Glycation | 1 | K·1 (track-only) |

**CDR location weights:** HCDR1·2, HCDR2·4, HCDR3·4, LCDR1·2, LCDR2·1, LCDR3·3.

**Critical rules:**
- **N-P-S/T sequons are NOT flagged** — proline at X blocks canonical glycan transfer
  (Gavel & von Heijne 1990). `NPS`/`NPT` retained only as reference constants.
- **Track-only liabilities** `{(Unpaired_Cys, C), (Isomerization, DT), (Isomerization, DH),
  (Glycation, K)}` are recorded but excluded from the active flag total.
- The `N[^P][ST]` regex cannot be used in the position-level `ptm_scan` — use the
  pre-expanded `NGLYCOSYLATION_SCAN_MOTIFS` (19 non-Pro AAs × {S,T} = 38 triplets).

---

## 3. ESM substitution prediction (inverse folding)

RAPID's ESM scan uses an **ensemble of 6 Facebook models** —
`["esm1b", "esm1v1", "esm1v2", "esm1v3", "esm1v4", "esm1v5"]` (UR90S, ~650M params each)
— via `home/protein_llms/kim_models/`:

```python
from home.protein_llms.kim_models.amis import reconstruct_multi_models
muts = reconstruct_multi_models(sequence, model_names, alpha=None)  # favorable subs
```

Inverse-folding reconstruction proposes substitutions the language model scores as more
native-like; these become candidate mutations alongside the liability/germline scans.

---

## 4. BLOSUM62-ranked substitution + MHC-II risk

`automated_r0/substitution.py` proposes replacements at each flagged position:

- Generate the 20 standard AAs **minus `DEFAULT_EXCLUDED_AAS`** (C, D, N, M) minus self →
  ~15 candidates evaluated for MHC-II.
- Rank by **BLOSUM62** similarity to wild-type.
- **At liability sites** (`exhaustive_positions`): no `BLOSUM62 >= 0` filter, no `top_n` cap
  — evaluate all. **Elsewhere:** apply `BLOSUM62 >= 0` + `top_n`.
- Each candidate's **MHC-II binding-risk delta** is computed via the insilico-suite
  (`mhc_ii.binding.compute_binding_risk_deltas`) — see `references/mhc-binding-prediction.md`.

---

## 5. The RAPID pipeline (7 stages, checkpoint-resumable)

`automated_r0/management/commands/rapid_pipeline.py`. Input = a GDB **TPP ID** (or raw
VL/VH sequences); output = an Excel workbook of ranked variants per chain.

| # | Stage | Does |
|---|---|---|
| 1 | `fetch_parse` | Pull GenBank from GDB (Genedata), parse LOCUS/features/ORIGIN, infer chain type, extract V-region + CDR positions |
| 2 | `fasta` | Emit combined + per-chain FASTA (`{TPP}.fst`, `{TPP}_parental_L.fst`, `_H.fst`) |
| 3 | `humanness` | AbRSA CDR annotation + ESM-embedding % humanness + germline ID (IGKV/IGLV/IGHV via BLAST) |
| 4 | `scans_excel` | 4 scans on V-region: **alanine**, **germline-revert**, **PTM-liability**, **ESM** → Excel (VL/VH sheets) |
| 5 | `substitution` | Per-flag candidate generation, BLOSUM62 ranking (§4) → per-scan CSVs |
| 6 | `batched_mhcii` | Dispatch mutants to insilico-suite MHC-II serving (Databricks), batched (~8 parallel), HLA class-II allele set |
| 7 | `risk_and_flags` | Aggregate MHC-II deltas back into workbook; combine PTM + binding risk; finalize flags |

Resumption state lives in `.rapid_checkpoint.json`. Excel columns per chain: Name, Original
TPP, Chain, Mutation, Scan Type, MHC-II Score, Total Flags, Non-Germline Flags, New
Sequence, then per-position liability flags.

---

## External tools & services (subprocess / remote)

| Tool | Where | Purpose |
|---|---|---|
| **NetMHCIIpan 4.3** (EL) | insilico-suite `mhc_ii/`, bundled binary | MHC-II T-cell binding (see mhc-binding-prediction.md) |
| **IgBLAST 1.22** | bundled (`ncbi-igblast-1.22.0/`), via ig-align | germline V(D)J alignment |
| **rpsblast + CDD** | via `IgCdSearch` | Ig domain detection (AbRSA backend) |
| **MMseqs2 18-8cc5c** | insilico-suite `pathogen_mimicry/` | k-mer homology (pathogen mimicry, off-target) |
| **GDB (Genedata)** | `local_packages.gdb_client.GDBClient` | TPP metadata + GenBank fetch |
| **Databricks serving** | `insilico_suite.pipelines` | remote MHC-II binding prediction |
| **ESM-1v/1b** | `fair-esm`, local (~7 GB cache) | substitution prediction |

---

## Related

- `references/mhc-binding-prediction.md` — MHC-II binding (the immunogenicity input RAPID consumes)
- `references/immunogenicity-prediction.md` — multi-factor T-cell response scoring
- `bio-alignment` / `bio-database-access` (genetics umbrella) — germline BLAST, homology search
- `digitalbtd-insilico-suite/CLAUDE.md` and `digitalbtd-portal/src/automated_r0/` — source of truth for the live pipelines
