# Homology Search — Remote BLAST, PSI-BLAST, HMMER, RBH, Delta-BLAST

> Source: former `bio-homology-search` skill (primary tool: `Bio.Blast.NCBIWWW` + BLAST+; mixed CLI/Python).

## Version Compatibility

Reference examples tested with: BioPython 1.83+, NCBI BLAST+ 2.15+, HMMER 3.3+

Before using code patterns, verify installed versions match. If versions differ:
- Python: `pip show <package>` then `help(module.function)` to check signatures
- CLI: `<tool> --version` then `<tool> --help` to confirm flags

If code throws ImportError, AttributeError, or TypeError, introspect the installed
package and adapt the example to match the actual API rather than retrying.

# Homology Search

Find sequences related to a query — from quick remote BLAST against NCBI, to
sensitive iterative and profile-based methods for detecting distant homologs and
calling orthologs.

Trigger keywords: BLAST, blastn, blastp, blastx, homology, homolog, PSI-BLAST,
HMMER, jackhmmer, hmmsearch, hmmscan, ortholog, reciprocal best hit, RBH,
Delta-BLAST, remote search, Pfam.

## When to Use

```
What kind of homology search?
├── One/few sequences, no local DB, occasional       → Remote BLAST (NCBIWWW.qblast)
├── Distant homologs missed by standard BLAST         → PSI-BLAST (iterative PSSM)
├── Protein family / domain detection (Pfam)          → HMMER (hmmsearch/hmmscan)
├── Most sensitive iterative profile search           → jackhmmer
├── 1:1 orthologs between two proteomes               → Reciprocal best hits (RBH)
├── Domain-aware first pass                            → Delta-BLAST
└── Many queries, custom DB, large-scale, fast/offline → see bio-local-blast skill
```

`bio-local-blast` (a separate skill) covers the BLAST+ command-line workflow:
`makeblastdb`, local `blastn`/`blastp`, custom databases, and tabular output for
fast, unlimited, offline searches.

---

# Remote BLAST (Bio.Blast)

Submit a query to NCBI's BLAST servers, retrieve XML, and parse hits.

## Required Import

```python
from Bio.Blast import NCBIWWW, NCBIXML
from Bio import SeqIO
```

## BLAST Programs

| Program | Query | Database | Use Case |
|---------|-------|----------|----------|
| `blastn` | Nucleotide | Nucleotide | DNA/RNA sequence similarity |
| `blastp` | Protein | Protein | Protein sequence similarity |
| `blastx` | Nucleotide | Protein | Find protein hits for DNA query |
| `tblastn` | Protein | Nucleotide | Find DNA encoding protein-like |
| `tblastx` | Nucleotide | Nucleotide | Translated vs translated |

## NCBIWWW.qblast()

```python
from Bio.Blast import NCBIWWW
result_handle = NCBIWWW.qblast('blastn', 'nt', sequence)
```

**Key Parameters:**
| Parameter | Description | Example |
|-----------|-------------|---------|
| `program` | BLAST program | `'blastn'`, `'blastp'` |
| `database` | Target database | `'nr'`, `'nt'`, `'refseq_rna'` |
| `sequence` | Query sequence | String or SeqRecord |
| `entrez_query` | Limit by Entrez query | `'Homo sapiens[organism]'` |
| `hitlist_size` | Max hits to return | `50` |
| `expect` | E-value threshold | `0.001` |
| `word_size` | Word size | `11` for blastn |
| `gapcosts` | Gap penalties | `'5 2'` (open, extend) |
| `format_type` | Output format | `'XML'` (default), `'Text'` |

## Common Databases

**Nucleotide:** `nt` (all GenBank+EMBL+DDBJ), `refseq_rna`, `refseq_genomic`
**Protein:** `nr` (non-redundant), `refseq_protein`, `swissprot` (curated), `pdb` (structures)

## Parsing Results

```python
from Bio.Blast import NCBIWWW, NCBIXML

result_handle = NCBIWWW.qblast('blastn', 'nt', sequence)
blast_record = NCBIXML.read(result_handle)   # one query; .parse() for many
result_handle.close()

for alignment in blast_record.alignments:
    print(f"Hit: {alignment.title}")
    for hsp in alignment.hsps:
        print(f"  E-value: {hsp.expect}")
        print(f"  Identity: {hsp.identities}/{hsp.align_length}")
```

**Key attributes** — `alignment`: `.title`, `.accession`, `.length`, `.hsps`.
`hsp` (High-scoring Segment Pair): `.score`, `.bits`, `.expect`, `.identities`,
`.positives`, `.gaps`, `.align_length`, `.query`/`.match`/`.sbjct`,
`.query_start`/`.query_end`, `.sbjct_start`/`.sbjct_end`, `.strand`, `.frame`.

## Timing Notes

- Remote BLAST can take 30 seconds to several minutes; longer queries take longer.
- Peak times (US business hours) may be slower.
- For many queries or speed, use the **bio-local-blast** skill.

Basic blastn/blastp/blastx runs, FASTA-file input, save-to-file, top-hit
extraction, identity/coverage filtering, and multi-sequence loops are in the
Usage Guide below.

---

# PSI-BLAST (Iterative PSSM)

Builds a position-specific scoring matrix through iterations to find distant
homologs that standard BLAST misses.

```bash
# Basic iterative search
psiblast -query protein.fasta -db nr -out results.txt -num_iterations 3

# Save PSSM for reuse, then reuse it
psiblast -query protein.fasta -db nr -out_pssm pssm.asn -out_ascii_pssm pssm.txt -num_iterations 5
psiblast -in_pssm pssm.asn -db nr -out results.txt

# Tabular output with inclusion threshold
psiblast -query protein.fasta -db nr -outfmt 6 -num_iterations 3 -inclusion_ethresh 0.001
```

| Parameter | Default | Description |
|-----------|---------|-------------|
| `-num_iterations` | 1 | Number of iterations (use 3-5) |
| `-inclusion_ethresh` | 0.002 | E-value for PSSM inclusion (0.001 = cleaner) |
| `-evalue` | 10 | E-value threshold for reporting |
| `-num_threads` | 1 | CPU threads |

Iterative remote PSI-BLAST via Biopython is in the Usage Guide below.

---

# HMMER / jackhmmer (Profile HMMs)

Profile hidden Markov models give the most sensitive searches for protein
families and remote relationships.

```bash
# Search a database with a single query sequence
jackhmmer -o results.txt -A aligned.sto --cpu 8 query.fasta database.fasta

# Build a profile from an alignment, then search a database
hmmbuild profile.hmm alignment.sto
hmmsearch -o results.txt --tblout hits.tbl profile.hmm database.fasta
hmmsearch -o results.txt --domtblout domains.tbl profile.hmm database.fasta

# Download Pfam and scan a sequence against it
wget https://ftp.ebi.ac.uk/pub/databases/Pfam/current_release/Pfam-A.hmm.gz
gunzip Pfam-A.hmm.gz && hmmpress Pfam-A.hmm
hmmscan --tblout pfam_hits.tbl --domtblout domains.tbl Pfam-A.hmm query.fasta

# Iterative HMMER (PSI-BLAST analogue)
jackhmmer -N 5 -o results.txt --tblout hits.tbl query.fasta database.fasta
```

`--tblout` columns: 1 target name, 2 accession, 3 query name, 4 query accession,
5 E-value (full seq), 6 score (full seq), 7 bias, 8 E-value (best domain),
9 score (best domain).

```bash
# Quick filtering of tabular output
grep -v "^#" hits.tbl | head
awk '$5 < 1e-10' hits.tbl
```

Use **hmmscan** for domain identification, **hmmsearch** for protein-family
members. Parsing HMMER output with `Bio.SearchIO` is in the Usage Guide below.

---

# Reciprocal Best Hits (Orthologs)

Find 1:1 orthologs between two species via bidirectional best hits.

```bash
# Build databases
makeblastdb -in species_A.fasta -dbtype prot -out species_A_db
makeblastdb -in species_B.fasta -dbtype prot -out species_B_db

# Bidirectional best-hit BLAST
blastp -query species_A.fasta -db species_B_db -outfmt 6 -evalue 1e-5 -max_target_seqs 1 > A_vs_B.txt
blastp -query species_B.fasta -db species_A_db -outfmt 6 -evalue 1e-5 -max_target_seqs 1 > B_vs_A.txt

# Reciprocal pairs
awk 'FNR==NR {a[$1]=$2; next} $2 in a && a[$2]==$1 {print $1"\t"$2}' \
    A_vs_B.txt B_vs_A.txt > reciprocal_best_hits.txt
```

For multi-species orthology, **OrthoFinder** automates the all-vs-all RBH and
orthogroup inference:

```bash
orthofinder -f proteomes/ -t 8          # add -M msa for MSA-based gene trees
```

A Python RBH parser, a complete shell ortholog pipeline, and OrthoFinder output
notes are in the Usage Guide below.

---

# Other Advanced Methods

```bash
# Delta-BLAST: conserved-domain DB for a more sensitive initial search
deltablast -query protein.fasta -db nr -rpsdb cdd_delta -out results.txt

# PHI-BLAST: pattern + sequence
phiblast -query protein.fasta -db nr -pattern "G-x(2)-[ST]-x-[RK]" -out results.txt
```

## Method Comparison

| Method | Speed | Sensitivity | Use Case |
|--------|-------|-------------|----------|
| BLASTP | Fast | Moderate | Close homologs |
| PSI-BLAST | Medium | High | Remote homologs |
| HMMER | Slow | Highest | Protein families |
| Delta-BLAST | Medium | High | Domain-aware |

## E-value vs Bit Score

| E-value | Interpretation |
|---------|----------------|
| < 1e-50 | Highly significant, likely homolog |
| 1e-50 to 1e-10 | Significant, probable homolog |
| 1e-10 to 1e-3 | Marginal, possible remote homolog |
| > 0.01 | Not significant |

---

## Common Errors

| Error | Cause | Solution |
|-------|-------|----------|
| Timeout (remote) | NCBI servers busy | Retry later, use smaller query, or go local |
| No hits | Wrong database or program | Check program/database match |
| Empty results | E-value too stringent | Increase `expect` / `-evalue` |
| `URLError` | Network issue | Check connection, retry |
| Empty PSSM / no new hits | Inclusion threshold too strict | Loosen `-inclusion_ethresh` |
| HMMER `--tblout` empty | No hits above threshold | Inspect with no E-value filter first |
| Spurious RBH pairs | E-value too loose | Tighten `-evalue` (e.g. 1e-10) |

## Tips

- Run PSI-BLAST for 3-5 iterations; use a lower inclusion E-value (0.001) for cleaner profiles.
- HMMER is best for very distant relationships and family membership.
- Always validate ortholog calls with reciprocal searches.

---

# Usage Guide (deep code patterns)

## Overview

This skill enables AI agents to find homologous sequences — from quick remote
NCBI BLAST to advanced methods (PSI-BLAST, HMMER, reciprocal best hits). For the
local BLAST+ CLI workflow, see the separate **bio-local-blast** skill.

## Prerequisites

```bash
pip install biopython              # remote BLAST + parsing
conda install -c bioconda blast    # BLAST+ (psiblast, makeblastdb, deltablast)
conda install -c bioconda hmmer    # HMMER (hmmsearch, hmmscan, jackhmmer)
conda install -c bioconda orthofinder  # ortholog identification
```

## Quick Start

Tell your AI agent what you want to do:

- "BLAST this sequence against the nr database"
- "Find what organism this DNA sequence comes from"
- "Run PSI-BLAST for 5 iterations to find remote homologs of this protein"
- "Search my protein against Pfam to identify conserved domains"
- "Find 1:1 orthologs between human and mouse proteomes"

## What the Agent Will Do

1. Select the method by sensitivity / scale (remote BLAST, PSI-BLAST, HMMER, RBH)
2. Choose the right program and database
3. Configure parameters (iterations, E-values, threads)
4. Run the search and parse/filter results by significance
5. Interpret homology relationships (orthologs, paralogs, domains)

---

## Remote BLAST Patterns

### Basic BLASTN

```python
from Bio.Blast import NCBIWWW, NCBIXML

sequence = '''ATGAAAGCAATTTTCGTACTGAAAGGTTGGTGGCGCACTTCCTGA'''
print("Running BLASTN (this may take a minute)...")
result_handle = NCBIWWW.qblast('blastn', 'nt', sequence)
blast_record = NCBIXML.read(result_handle)
result_handle.close()

print(f"\nFound {len(blast_record.alignments)} hits")
for alignment in blast_record.alignments[:5]:
    hsp = alignment.hsps[0]
    print(f"\n{alignment.title[:70]}...")
    print(f"  E-value: {hsp.expect:.2e}")
    print(f"  Identity: {hsp.identities}/{hsp.align_length} ({100*hsp.identities/hsp.align_length:.1f}%)")
```

### BLASTP with Organism Filter

```python
result_handle = NCBIWWW.qblast(
    'blastp', 'nr', protein_seq,
    entrez_query='Mammalia[organism]',
    hitlist_size=20, expect=0.001,
)
blast_record = NCBIXML.read(result_handle)
result_handle.close()
for alignment in blast_record.alignments[:10]:
    hsp = alignment.hsps[0]
    print(f"{alignment.accession}: E={hsp.expect:.2e} - {alignment.title[:50]}...")
```

### BLAST from a FASTA File

```python
from Bio import SeqIO
from Bio.Blast import NCBIWWW, NCBIXML

record = SeqIO.read('query.fasta', 'fasta')
result_handle = NCBIWWW.qblast('blastn', 'nt', record.seq)
blast_record = NCBIXML.read(result_handle)
result_handle.close()
```

### BLASTX (DNA query → protein DB)

```python
result_handle = NCBIWWW.qblast('blastx', 'nr', dna_sequence)
blast_record = NCBIXML.read(result_handle)
result_handle.close()
for alignment in blast_record.alignments[:5]:
    hsp = alignment.hsps[0]
    print(f"{alignment.accession}: frame {hsp.frame}, E={hsp.expect:.2e}")
```

### Save Results to File, Parse Later

```python
result_handle = NCBIWWW.qblast('blastn', 'nt', sequence)
with open('blast_results.xml', 'w') as out:
    out.write(result_handle.read())
result_handle.close()

from Bio.Blast import NCBIXML
with open('blast_results.xml') as f:
    blast_record = NCBIXML.read(f)
```

### Extract Top Hits (structured)

```python
def get_top_hits(sequence, program='blastn', database='nt', num_hits=10, evalue=0.01):
    result_handle = NCBIWWW.qblast(program, database, sequence,
                                   hitlist_size=num_hits, expect=evalue)
    blast_record = NCBIXML.read(result_handle)
    result_handle.close()
    hits = []
    for alignment in blast_record.alignments:
        hsp = alignment.hsps[0]
        hits.append({
            'accession': alignment.accession,
            'title': alignment.title,
            'evalue': hsp.expect,
            'identity': hsp.identities / hsp.align_length,
            'coverage': hsp.align_length / blast_record.query_length,
        })
    return hits
```

### Filter by Identity / Coverage

```python
def filter_blast_hits(blast_record, min_identity=0.9, min_coverage=0.8):
    query_length = blast_record.query_length
    filtered = []
    for alignment in blast_record.alignments:
        for hsp in alignment.hsps:
            identity = hsp.identities / hsp.align_length
            coverage = hsp.align_length / query_length
            if identity >= min_identity and coverage >= min_coverage:
                filtered.append({
                    'accession': alignment.accession,
                    'title': alignment.title,
                    'identity': identity,
                    'coverage': coverage,
                    'evalue': hsp.expect,
                })
    return filtered
```

### Multiple Sequences (be nice to NCBI)

```python
from Bio import SeqIO
from Bio.Blast import NCBIWWW, NCBIXML
import time

def blast_multiple(fasta_file, program='blastn', database='nt'):
    results = {}
    for record in SeqIO.parse(fasta_file, 'fasta'):
        print(f"BLASTing {record.id}...")
        result_handle = NCBIWWW.qblast(program, database, str(record.seq), hitlist_size=5)
        results[record.id] = NCBIXML.read(result_handle)
        result_handle.close()
        time.sleep(5)
    return results
```

---

## PSI-BLAST (Remote, via Biopython)

```python
from Bio.Blast import NCBIWWW, NCBIXML

with open('query.fasta') as f:
    query = f.read()

result_handle = NCBIWWW.qblast('psiblast', 'nr', query, expect=0.001, word_size=3)
with open('psiblast_result.xml', 'w') as out:
    out.write(result_handle.read())
result_handle.close()

with open('psiblast_result.xml') as f:
    for record in NCBIXML.parse(f):
        for alignment in record.alignments:
            for hsp in alignment.hsps:
                if hsp.expect < 1e-10:
                    print(f'{alignment.hit_def[:50]}: E={hsp.expect}')
```

For local iterative PSI-BLAST with PSSM save/reuse, see the CLI snippets above.

---

## HMMER Parsing (Bio.SearchIO)

```python
from Bio import SearchIO

results = SearchIO.parse('hmmsearch_output.txt', 'hmmer3-text')
for query_result in results:
    print(f'Query: {query_result.id}')
    for hit in query_result:
        print(f'  Hit: {hit.id}, E-value: {hit.evalue}')
        for hsp in hit:
            print(f'    Domain: {hsp.bitscore} bits')
```

---

## Reciprocal Best Hits

### Python RBH Parser

```python
def find_rbh(forward_blast, reverse_blast):
    '''Find reciprocal best hits from two outfmt-6 BLAST result files.'''
    forward = {}
    with open(forward_blast) as f:
        for line in f:
            query, subject = line.strip().split('\t')[:2]
            forward.setdefault(query, subject)

    reverse = {}
    with open(reverse_blast) as f:
        for line in f:
            query, subject = line.strip().split('\t')[:2]
            reverse.setdefault(query, subject)

    return [(a, b) for a, b in forward.items()
            if b in reverse and reverse[b] == a]

rbh_pairs = find_rbh('A_vs_B.txt', 'B_vs_A.txt')
```

### Complete Shell Ortholog Pipeline

```bash
#!/bin/bash
SPECIES_A=$1
SPECIES_B=$2
EVALUE=1e-10
THREADS=8

echo "Building databases..."
makeblastdb -in $SPECIES_A -dbtype prot -out db_A
makeblastdb -in $SPECIES_B -dbtype prot -out db_B

echo "Running forward BLAST..."
blastp -query $SPECIES_A -db db_B -outfmt 6 -evalue $EVALUE \
    -max_target_seqs 1 -num_threads $THREADS > forward.txt

echo "Running reverse BLAST..."
blastp -query $SPECIES_B -db db_A -outfmt 6 -evalue $EVALUE \
    -max_target_seqs 1 -num_threads $THREADS > reverse.txt

echo "Finding reciprocal best hits..."
awk 'FNR==NR {best[$1]=$2; next}
     $2 in best && best[$2]==$1 {print $1"\t"$2}' \
     forward.txt reverse.txt > orthologs.txt

echo "Found $(wc -l < orthologs.txt) ortholog pairs"
rm -f db_A.* db_B.*
```

### OrthoFinder Output Files

| File | Content |
|------|---------|
| Orthogroups.tsv | All orthogroups |
| Orthogroups_SingleCopyOrthologues.txt | 1:1 orthologs |
| Species_Tree/ | Inferred species tree |
| Gene_Trees/ | Individual gene trees |

```bash
mkdir proteomes && cp species_*.fasta proteomes/
orthofinder -f proteomes/ -t 8
```

## Tips

- Remote BLAST takes 30 sec to several minutes; lower E-value = more significant hit.
- Specify an organism (`entrez_query`) to narrow remote results; save results to file if needed later.
- For many queries or speed, switch to the bio-local-blast skill.
- PSI-BLAST 3-5 iterations with inclusion E-value 0.001 gives clean, sensitive profiles.
- Use hmmscan for domain identification, hmmsearch for protein-family members.
