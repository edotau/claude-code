---
name: bio-immunoinformatics
description: Immunoinformatics + therapeutic-antibody engineering toolkit. Antibody de-risking — CDR annotation/numbering (ig-align/AbRSA, IMGT/Kabat/Chothia, germline ID), developability liability scanning (deamidation, isomerization, N-glycosylation sequons, unpaired Cys, oxidation; CDR-position weights), ESM-1v substitution prediction, and the RAPID R0-triage pipeline. Peptide-MHC class I/II binding affinity (MHCflurry, NetMHCpan/NetMHCIIpan, IC50/percentile rank, HLA alleles); B-cell and T-cell epitope prediction (BepiPred, IEDB, DiscoTope/ElliPro); peptide immunogenicity scoring; plus tumor neoantigen (pVACtools/pvacseq, VEP VCF) and TCR-epitope recognition (ERGO-II, VDJdb). Trigger on antibody, CDR, AbRSA, ig-align, humanness, germline, developability, liability, deamidation, isomerization, glycosylation, ESM, RAPID, MHC binding, NetMHCIIpan, MHCflurry, HLA, epitope, immunogenicity, neoantigen, TCR, vaccine design.
---
# Bio-Immunoinformatics

Router skill for therapeutic-antibody engineering and T-cell/B-cell immunoinformatics:
antibody de-risking (CDR annotation, developability liabilities, substitution, RAPID
triage), MHC binding, epitope discovery, immunogenicity scoring, and — secondarily — TCR
recognition and tumor neoantigen prediction. This file is a navigable index — pick a task
below, then open the matching `references/*.md` file for the full technical content
(runnable code, version-compatibility notes, parameters, thresholds).

**BI focus:** the primary workflow here is therapeutic-antibody de-risking as used in
`digitalbtd-insilico-suite` and `digitalbtd-portal/src/automated_r0` (RAPID). Start at
`references/antibody-engineering.md`, which feeds MHC-II binding
(`references/mhc-binding-prediction.md`).

## How the pieces fit together

**Antibody de-risking (primary):** annotate CDRs → scan developability liabilities →
propose substitutions (BLOSUM62 + ESM) → score each variant's **MHC-II immunogenicity**
→ rank candidates (the RAPID 7-stage pipeline).
**General T-cell pipeline:** scan a protein for binders (MHC binding) → identify epitopes
→ rank by immunogenicity → (tumors only) derive neoepitopes from somatic mutations. MHC
binding affinity is one input factor consumed by the immunogenicity stage.

## Task → reference map

| If you want to… | Open |
|-----------------|------|
| **(BI primary)** Annotate antibody CDRs/numbering (ig-align/AbRSA, IMGT/Kabat/Chothia, germline ID); scan developability liabilities (deamidation, isomerization, N-glyc sequons, unpaired Cys, oxidation; CDR weights); ESM-1v substitution; run/understand the **RAPID** R0-triage pipeline | `references/antibody-engineering.md` |
| Predict peptide-MHC class I/II binding affinity (IC50, percentile rank) with MHCflurry or NetMHCpan/NetMHCIIpan; scan proteins; interpret strong/moderate/weak; common HLA allele sets | `references/mhc-binding-prediction.md` |
| Predict B-cell epitopes (BepiPred/IEDB), linear vs conformational (DiscoTope/ElliPro), consensus scoring, peptide-array mapping; T-cell epitopes via IEDB MHC-I | `references/epitope-prediction.md` |
| Score peptide immunogenicity (multi-factor weighted model, processing, self-similarity/foreignness, anchor quality, tiering, vaccine-candidate selection) | `references/immunogenicity-prediction.md` |
| _(secondary)_ TCR-epitope recognition (ERGO-II, VDJdb matching, clustering, repertoire) — also in immunogenicity-prediction | `references/immunogenicity-prediction.md` |
| _(secondary)_ Tumor neoantigens from somatic mutations: pVACtools/pvacseq (VEP-annotated VCF, agretopicity/DAI, prioritization, cyvcf2) | `references/neoantigen-prediction.md` |

## When to use which

- **Engineering an antibody candidate** (CDRs, liabilities, humanness, variant ranking, RAPID) → **antibody-engineering** (the BI default).
- **Just need binding affinity** (does peptide X bind HLA-DRB1\*?) → mhc-binding-prediction.
- **Need to find epitopes in an antigen** → epitope-prediction.
- **Rank candidates / assess T-cell response / match TCRs** → immunogenicity-prediction.
- **Starting from a tumor VCF** (not typical for BI antibody work) → neoantigen-prediction.

> **Scope note:** the neoantigen (`neoantigen-prediction.md`) and TCR-recognition
> (in `immunogenicity-prediction.md`) references are kept but de-emphasized — they are not
> used by the BI antibody de-risking workflow.

## Common tooling

- **ig-align / AbRSA** — antibody CDR annotation, numbering (abm/kabat/chothia/imgt),
  germline closeness, Ig-domain detection. Private Nexus pkg (`ig-align==0.4rc5`); replaces
  anarci/abnumber. See `references/antibody-engineering.md`.
- **NetMHCIIpan 4.3** — MHC-II binding (bundled in insilico-suite); the immunogenicity
  input the RAPID pipeline consumes.
- **fair-esm** (`esm1b`/`esm1v1-5` ensemble) — inverse-folding substitution prediction.
- **MHCflurry** — class I binding + processing (`Class1PresentationPredictor`,
  `Class1ProcessingPredictor`). Run `mhcflurry-downloads fetch` once before loading.
- **IEDB APIs** — B-cell (`/tools_api/bcell/`), MHC-I (`/tools_api/mhci/`), MHC-II
  (`/tools_api/mhcii/`, NetMHCIIpan). No local install.
- **pVACtools** — `pvacseq run` on a VEP-annotated VCF (`Downstream` + `Wildtype`
  plugins required); `pvactools download_iedb_tools`.
- **ERGO-II / VDJdb** — TCR-epitope recognition (clone ERGO-II repo; download VDJdb TSV).
- **PepX (IEDB)** — expression annotation feeding the immunogenicity score (~120 GB SQLite
  DB, `PEPX_DB_PATH`).

## Version compatibility (all references)

Each reference file restates the exact versions its examples were tested against
(MHCflurry 2.1+, pVACtools 4.1+, Ensembl VEP 111+, ERGO-II, MiXCR 4.6+, pandas 2.2+,
numpy 1.26+, scikit-learn 1.4+, scipy 1.12+). If any example raises ImportError,
AttributeError, or TypeError, introspect the installed package (`pip show <pkg>`,
`help(module.function)`, `<tool> --help`) and adapt the call rather than retrying.

## Related skills

- `genetics` umbrella → `bio-database-access` (UniProt peptides for self-similarity,
  NCBI/Entrez) and `bio-alignment` (homology search behind pathogen-mimicry workflows)
- `machine-learning` umbrella → `transformers` (ESM model loading), `scikit-learn`
- `bio-proteomics` — mass-spec confirmation of PTM liabilities
