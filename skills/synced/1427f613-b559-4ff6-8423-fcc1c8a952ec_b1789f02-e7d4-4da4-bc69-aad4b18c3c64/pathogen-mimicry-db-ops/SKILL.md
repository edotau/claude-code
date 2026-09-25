---
name: pathogen-mimicry-db-ops
description: "House conventions for building and annotating the MMseqs2 pathogen-mimicry databases (GTDB, CNGB, UniProtKB, NCBI NR, EMBL/MGnify) in digitalbtd-insilico-suite, and for submitting that work to Databricks as a batch job. Covers UC Volume tiering, s5cmd vs FUSE transfers, mmseqs DB identity rules, the annotation-provenance contract, eggNOG/KEGG annotation routes, and the two working job-submission shapes. Load before editing scripts/mmseqs, adding an annotate-db or cluster-db route, or submitting a Spark/wheel run."
metadata:
  authored_via: "claude-science"
  published_by: "agent"
  published_at: "2026-09-11T09:32:25.869Z"
  authored_by: "agent"
  authored_at: "2026-09-11T02:49:15.727Z"
  authoring_session: "28a5d7a3-1589-46a4-adee-5a797e1c3fb9"
  last_modified_by: "agent"
  last_modified_at: "2026-09-11T09:30:31.276Z"
---

# Pathogen-mimicry database ops

Conventions for `digitalbtd-insilico-suite`. Everything here was verified against the
repo or a live run; where a claim is inferred rather than observed it says so.

## Why annotation quality is the product

The mimicry search tiles a candidate into 7-15mers and matches them against billions of
residues. Every hit is at or below chance expectation by construction — measured best
E-values ran 0.38 (gtdb) to 36.0 (cngb), and `pct_id` is saturated at 100 on essentially
every row. So the hit is not the evidence. **The annotation is what makes a hit
reviewable**: "a locus in an unnamed genome" is unactionable, "a streptococcal surface
antigen" is a flag worth a human. Treat annotation coverage as the deliverable, not as
metadata about one.

Corollary: a coverage gate must count *informative* rows, not annotated ones. A pass that
resolves every sequence to "hypothetical protein" clears an annotated/total gate and
changes nothing.

## Where things live

Three tiers, and the separation is load-bearing:

| tier | path | who writes |
|---|---|---|
| work | `/local_disk0/<...>` | the running job; node-local NVMe on the i3 shapes |
| staging | `/Volumes/digitalbtd-dev/insilico/databases_reclustered/<db>` | every run, unconditionally |
| live | `/Volumes/digitalbtd-dev/insilico/databases/<db>` | only behind an explicit `--upload` / `UPLOAD=1` |

Use `VolumePaths` in `insilico_suite/configs.py` (`.databases`, `.databases_reclustered`,
`.job_storage`) rather than re-literalling these paths per script — several scripts each
carried their own copy, which is one edit away from a build publishing into the live volume.

Also on the job_storage volume: `binaries/` (mmseqs, s5cmd), `scripts/` (staged `.sh`
and the cluster init script), `wheels/` (Nexus-only wheels the cluster cannot fetch itself).

## Transfers: s5cmd direct to S3, not FUSE

The house standard is `s5cmd --numworkers 256 cp`, against the S3 object store, in both
directions. A FUSE `cp` is single-streamed. Volume UUIDs are read off
`databricks volumes read digitalbtd-dev.insilico.<name>` — never guessed.

FUSE also fails outright on some access patterns, and the failures are obscure:

- `tee -a` onto a Volume path dies with "Illegal seek". Write logs node-local, copy to the
  Volume on exit (a fresh sequential write, which works).
- Tools that seek in their own output (eggnog-mapper does) must use a node-local
  `--output_dir`, then copy the finished file.
- A shard written by the driver onto `/local_disk0` is invisible to the executor that has
  to read it. Anything crossing the driver/executor boundary lives on the Volume.

## MMseqs2 DB identity rules

**Sequence ids come from `<db>_h`, never `<db>.lookup`.** `createsubdb` and `result2repseq`
do not subset `.lookup` — they SYMLINK the parent's — so a clustered DB's lookup enumerates
the *pre-clustering* id set. Using it as a coverage denominator counts sequences the DB does
not contain. The sanctioned route:

```bash
mmseqs prefixid "${DB}_h" "${TMP}/hdr_prefixed" --tsv 1 --threads "${THREADS}" -v 1
# prefixid emits "<key>\t<full header>"; the accession is the header's first whitespace token
awk -F'\t' '{ split($2, a, /[ \t]/); if (a[1] != "") print $1 "\t" a[1] }' "${TMP}/hdr_prefixed"
```

A DB is a file set, not a file: `("", ".dbtype", ".index", ".lookup", ".source")` plus the
`("_h", "_mapping", "_taxonomy")` sidecars. Copy all of them or none.

**Per-database id normalization.** `CNGB_STRIP_PREFIXES` ("250twins_") is stripped from the
Target column by `target_id_cngb`, but upstream CNGB files use the prefixed spelling — 23%
of the KEGG annotation's genes. Any join must normalize both sides through the same constant.
A raw join silently drops those rows and reads as a worse upstream rather than a bug.

## The annotation-provenance contract

`pathogen_mimicry/metadata.py` owns four CLOSED enumerations — `ANNOTATION_SOURCES`,
`ANNOTATION_QUALITIES`, `ANNOTATION_REASONS`, `TAXONOMY_SOURCES` — plus the single
informativeness predicate `is_low_information_label`.

- Adding a new route means adding its source to `ANNOTATION_SOURCES` first. Assert
  membership at module import so a typo fails at load, not at the end of a 5M-row write.
- **Never re-implement the quality/reason derivation** in awk or SQL. `annotate_gtdb_eggnog.sh`
  does, and its own comment flags the drift hazard. Call `is_low_information_label`.
- Invariant §2.3: `annotation_reason` is non-null **iff** `annotation_quality != 'informative'`.
  Cheap to verify on a written table — count rows with an empty reason and compare to the
  informative count.
- The canonical header dialect (`format_canonical_header` / `parse_canonical_header`) carries
  `desc=`, `gene=`, `og=`, `src=` and **no reason field**, so an unannotated row round-trips
  to `empty_header` rather than its true reason. Lossless for the sidecar route; lossy for a
  header rewrite.

## Annotation routes, cheapest first

1. **An upstream annotation you are not reading.** Check the source's own FTP directory
   before designing any compute. CNGB publishes `humanGut_IGC.11M.KEGG.annotation.gz` beside
   the taxon annotation — 32 MB, took CNGB from 0 to ~4.58M informative descriptions with no
   search at all. It is reachable only over `ftp://` (the `https://` paths serve a JavaScript
   portal that a naive fetch stores as a `.gz`), and it is KO-major: one row per KO followed
   by every member gene, so it must be exploded before it can be joined.
2. **eggNOG via an mmseqs profile DB** (`build_eggnog_profile.sh`): 5.8 GB for
   bacteria+archaea, one driver, `mmseqs search` + a join against `e5.og_annotations.tsv`.
   Preferred over the emapper fan-out (47 GiB staged, 1024 shards, pinned pip emapper) because
   it is the engine already operated at scale. Sensitivity parity with emapper is **asserted
   in that script's header and not benchmarked** — measure on one shard before committing.
3. **Alignment transfer from NCBI NR / UniProt TrEMBL: tried, underperformed.** The reason is
   structural, not parametric — NR is largely gene-caller output, so a high-identity hit
   donates another "hypothetical protein" and the quality column does not move. Only transfer
   from a curated donor, and gate on `is_low_information_label` afterwards.

Annotation is **additive**: read the cluster DB, write new sidecar files beside it, never
recluster or rewrite. That is what makes an annotation pass safe without a rebuild.

## FTP sources have no HEAD and no Range

`download_resumable`'s Content-Length check cannot fire on an `ftp://` URL. Pass
`allow_unverified=True` and verify the payload instead — for a `.gz`, decompressing to check
the CRC32/length trailer is strictly stronger than a byte count.

## Submitting as a batch job

Two shapes work, for different workloads. Both are in the repo; copy the matching one.

### A. Driver-only work → wheel task on an EXISTING cluster

Use for anything mmseqs runs single-node (the builds, annotation joins): mmseqs is its own
parallel engine, so one fat driver is the whole workload. Reference:
`cmd_submit_cluster_db` / `cmd_submit_annotate_db` in `src/mmseqs_pipeline.py`.

- Attach via `cluster_id` to an interactive cluster — a job-compute cluster counts against
  the 3-running-clusters-per-user cap; a personal-compute one does not. The dev cluster is
  `mmseqs-db-rebuild-dev`, single-node i3.4xlarge, 16 cores / 125 GB, 90-min autotermination
  (verify the id before relying on it — clusters get recreated).
- The launcher is imported as a **NOTEBOOK**, not `spark_python_task`. Two constraints force
  it: a `python_wheel_task` resolves ig-align from Nexus (unreachable from a cluster), and the
  Jobs control plane cannot read a `spark_python_task` file from a UC Volume as the submitting
  service principal.
- Stage Nexus-only wheels into `job_storage/wheels/` and pass them in the notebook prologue's
  `EXTRA_WHEELS`. `mmseqs_pipeline` imports `bioinformatics.seq_cluster_registry` at module
  scope, which reaches ig-align, so an empty list fails at import on a clean cluster.
- A borrowed interactive cluster can be reaped mid-build by autotermination — that is what the
  Spark heartbeat in `clu_build_task.py` is for. A job cluster lives exactly as long as its run
  and needs no heartbeat.

### B. Embarrassingly parallel work → one-off `jobs submit` with a new cluster

Use only when the workload is millions of independent queries (the emapper fan-out).
Reference: `scripts/mmseqs/submit_gtdb_eggnog.sh`.

- `spark_python_task` off a **WORKSPACE FILE** (`--format AUTO` on a `.py` with no notebook
  magic header lands it as type FILE). A `/Volumes` path for `python_file` is rejected at
  launch even though the object is readable via the API.
- `num_workers` is pinned to 0-4 by the job policy; a submit asking for more is rejected at
  validation, and the policy also refuses 0 — so driver-only runs still ask for 1.
- Set `spark.task.cpus` = worker vCPUs, or Spark packs N tasks onto an N-core node and each
  one claims all cores. That is an OOM, not just contention.

### Both shapes

- Submissions run as the **service principal**, so the human who launched the run cannot even
  view it by default. Pass an `access_control_list` granting `CAN_MANAGE_RUN` — the CLI reads
  `DATABRICKS_RUN_VIEWERS`. Omitting it has stranded real runs.
- `DBX_ENV` / `DATABRICKS_ENV` selects the workspace, matching `resolve_dbx_env()`.
- Always offer a `--dry-run` that prints the payload and submits nothing.

## Deciding whether a candidate source is worth adopting

Run this BEFORE building any arm, index, or join. Three times now the decisive answer came from a
small download and an accession diff, and each time the intuitive answer was wrong. Building first
would have cost a job cycle to learn the same thing.

**1. Establish the join key empirically. Never assume an id space.**
Measured on CNGB's IGC 9.9M summary against our 11M-derived representatives:

| candidate key | result |
|---|---|
| `Gene Name` (col 2) | 76.4% match — the real key |
| `Gene ID` (col 1) | 0% — it is a row counter |
| trailing `GL` number alone | 2,941,858 hits on 65,994 ids — a 44x blowup, because gene numbers repeat across cohorts |

Sample a few thousand ids from the incumbent's own sidecar (`read(4_000_000)` off the descriptions
TSV is enough) and test EVERY plausible spelling, including re-prefixed forms, since normalizers
like `target_id_cngb` strip cohort prefixes that the upstream file still carries.

**2. Restrict the gain to rows that are currently unannotated, or the number is tautological.**
Export the incumbent's ids WITH their current verdict, then report the candidate's fill rate per
verdict. Unrestricted, CNGB's 9.9M summary looked like a +18-point win (eggNOG 60.4% vs KEGG 42.1%).
Restricted to `no_ortholog` rows it recovered exactly ZERO KOs — both catalogues draw on the same
KEGG assignment, which incidentally validated the incumbent's own coverage figure.

**3. Check what the candidate's "annotation" actually IS before counting it as coverage.**
That 60.4% eggNOG is orthologous-group ids plus one of 25 broad categories, a recurring value being
literally `Function unknown` (e.g. `bactNOG98109`). `is_low_information_label` scores those
uninformative, so they land as `cog_only` — annotated but not informative. A column being populated
is not the same as it carrying function.

**4. A catalogue diff answers "is there anything to gain", NOT "would we detect more".**
Say which question you answered. Pfam 38.2 vs CDD 3.21: all 8 canonical antibody families
(V-set, C1-set, I-set, C2-set, ig, Ig_2, Ig_3, C2-set_2) are already in CDD, so a Pfam arm adds
nothing for IgSF annotation. 465 of 845 E-set (clan **CL0159**, not CL0011) families are missing,
but that is a membership gap, not demonstrated recall.

**5. Report a keyword classifier as a noisy estimate, never a count.**
Flagging the 465 missing E-set families for pathogen relevance gave 28, with false positives visible
on inspection (`VERL_C` matched "envelope" but is a sea-urchin vitelline protein; `Ig_halo` matched
"bacteri" via *Halobacterial*, a non-pathogenic archaeon) and 333 left unclassified. Roughly ten
survive hand-checking. Give the hand-checked figure alongside the automated one.

**6. Ask whether the gain improves the incumbent's JOB or is a new capability.**
The pathogen Ig-like families CDD lacks (herpesvirus gp350, hantavirus Gn/Gc, reovirus lambda-2,
invasin, internalin J/K, Big-like) do not improve CDD's actual role, which is annotating the
antibody's own domains. They would be fold-level pathogen detection — a different arm, scoped and
benchmarked separately.

Small downloads that answer these without touching a cluster: `cdd.info` (CDD version and
per-source model counts), `cddid_all.tbl.gz` (every CDD accession, prefix identifies the source),
`family_superfamily_links` (superfamily membership, e.g. IgSF 472250 = 291 `cd*` + 6 `pfam` +
3 `smart`), `Pfam-A.clans.tsv.gz` (family to clan).

## Engine choice: mmseqs profile vs rpsblast

Measured on `scripts/pipeline/igsf_engine_bench.py`, n=1..896, 16 cores. Do not re-derive these.

- batched rpsblast: 0.38 s + 12.32 ms/record; bare profile search: 0.92 s + 0.17 ms/record.
- Arithmetic crossover is n=45, but 45-63 is a cost TIE (~0.93 s) where mmseqs gives up 4 of 18
  constant spans for <=0.24 s. `IGSF_CROSSOVER_FLOOR = 64` holds the exact arm to the top of that
  band on purpose. Do not "fix" it to 45.
- `createindex` + `--db-load-mode 2` cuts the 0.92 s fixed term to 0.22 s (0.76 s of it was the
  prefilter rebuilding the 291-profile table every call), moves the crossover to n=1, and wins at
  every N: 2.8x at 22, 13x at 352, 23x at 896, byte-identical output. At `-k 5`: 79 MB on disk,
  0.54 s to build, 13 MB packed, so it ships. `-k 6` is 890 MB, `-k 7` is 16 GB AND loses recall,
  `-k 4` is refused, `--split 4` is 2.9 GB and slower.
- `-s 7.5` is load-bearing: `-s 6.5` drops oracle recall 100% -> 97.4%.
- **The engines are NOT interchangeable for constant domains.** Concordance is exact on VARIABLE
  domains only (20/20 spans at n=22, 4000/4000 keys at n=4400, domain identity 100% at every N).
  ~9% of non-variable spans shift 1-3 residues (`IgC1_CH3_IgAEM_CH2_IgG`, `IgI_VEGFR`,
  `Ig5_Contactin`), and rpsblast is the arm matching the fixture oracle. Use rpsblast for CH2/CH3.
- A germline bake-off is a SEPARATE measurement (900 MB for 1.15x there); do not carry the IgSF
  index numbers over to it.

## Shell script conventions in scripts/mmseqs

- One mode per Spark stage (`--prepare` / `--shard I` / `--merge`), dispatched from a task `.py`.
- Every step sentinel-guarded with `.done.<step>` under `$WORK`, so a resubmit after a spot
  reclaim resumes instead of redoing finished work.
- Record the shard count at split time and refuse to merge if the ambient value disagrees —
  merging a subset is the failure that ships quietly.
- Gate on coverage before publishing, and mark the sentinel **after** the gate, so a resubmit
  re-runs the gate rather than skipping past it.
- Validate numeric env vars explicitly: a failed `[ "$X" -gt 0 ]` inside an `if` is exempt from
  `set -e`, so a non-integer silently disables the gate.
- `split -l` cuts FASTA records in half. Split by record with awk owning the boundary.

## CLI

`src/mmseqs_pipeline.py` is the entry point; subcommands dispatch through the `COMMANDS` dict
and the on-cluster runner shares the same module. Follow the `cluster-db` / `submit-cluster-db`
pair when adding a route — a local verb plus a `submit-` sibling that hands the same parameters
to Databricks, so the code that runs remote is the code under test locally.
