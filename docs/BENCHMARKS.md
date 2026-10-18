# Benchmarks

Throughput of xpro-oss with a **local-directory source** and a **SAP HANA**,
**PostgreSQL** or **Oracle** target. Results: [SAP HANA](#latest-results--2026-10-14) ·
[PostgreSQL](#postgresql--2026-10-14) · [Oracle and three-way comparison](#oracle--2026-10-14). HANA setup:
[HANA_DOCKER.md](HANA_DOCKER.md).
Measured with `cmd/bench`, which runs the same source → parser → target pipeline
as the service, in-process, and times `Pipeline.Run` (list + read + parse + insert).

Raw output of each sweep lives in [`docs/benchmarks/`](benchmarks/).

## Latest results — 2026-10-14

### Environment

| | |
|---|---|
| Host | 16 vCPU AMD EPYC-Genoa, 30 GB RAM, Debian 12, kernel 6.1 (cloud VM, shared test node) |
| Database | SAP HANA express 2.00.088.00 in Docker (root daemon), tenant `HXE`, same host |
| Connection | `localhost:39041`, TLS off, user `XPRO` |
| xpro-oss | built from this repo, linux/amd64, `CGO_ENABLED=0` |
| Table | 8 `NVARCHAR` columns + `raw_xml NCLOB` + timestamp (`db/migrations/001_init_hana.sql` shape) |
| Method | median of 3 runs per combination; table truncated before every run |
| Corpus | SAP ABAP-XML (`asx:abap`) billing documents, one unique ID per file |

The node was otherwise idle (application containers stopped) but HANA and
xpro-oss share the same 16 cores. Files were freshly written, so reads are
served from the page cache.

### Small files — 20,000 files, avg 19.3 KB, 387 MB

Files/s (higher is better); peak Go heap in MB in parentheses.

| workers | batch 50 | batch 100 | batch 500 |
|---:|---:|---:|---:|
| 1 | 827 (20) | 940 (14) | 1,030 (46) |
| 2 | 1,429 (20) | 1,588 (22) | 1,906 (87) |
| 4 | 2,838 (22) | 3,107 (38) | 3,533 (169) |
| 8 | 4,431 (39) | 5,192 (70) | 5,930 (338) |
| 16 | 6,310 (118) | 7,008 (175) | 7,230 (696) |
| 32 | 7,203 (249) | 7,709 (379) | 7,561 (1,350) |
| 64 | 7,511 (419) | **8,497** (627) | 8,393 (1,713) |
| 128 | 7,613 (666) | 8,221 (1,152) | **8,619** (1,685) |

### Large files — 6,000 files, avg 59.6 KB, 358 MB

| workers | batch 50 | batch 100 | batch 500 |
|---:|---:|---:|---:|
| 1 | 380 (18) | 404 (32) | 464 (132) |
| 2 | 679 (34) | 725 (60) | 877 (264) |
| 4 | 1,213 (59) | 1,410 (109) | 1,605 (530) |
| 8 | 2,093 (111) | 2,272 (216) | 2,297 (1,054) |
| 16 | 2,887 (291) | 3,249 (476) | 3,672 (1,573) |
| 32 | 3,384 (640) | 3,588 (891) | 3,743 (1,561) |
| 64 | 3,684 (1,006) | **3,883** (1,626) | 3,693 (1,564) |
| 128 | 3,387 (1,636) | 3,758 (1,643) | 3,751 (1,589) |

MB/s, seconds per run and failure counts are in the raw files. No run had any
failed or missing rows.

### Findings

* **Throughput plateaus around 16–32 workers.** Going from 1 to 16 workers is
  ~7.5x (small) / ~7.5–8x (large). Going from 32 to 128 adds at most ~14% (small, batch 500; typically 0–7%) and is
  close to run-to-run noise (large files even dip at 128). More concurrent
  connections do not help beyond that on this host: the limit is CPU shared with
  HANA, not xpro-oss's concurrency.
* **Ceiling on this host:** ~8,500 small files/s (~165 MB/s) and ~3,900 large
  files/s (~230 MB/s).
* **Batch size:** at low worker counts bigger batches are slightly faster; at
  16+ workers the gap narrows (≤ ~15%). Memory is the cost: peak heap grows with
  workers × batch size × file size (e.g. small files, 16 workers: 118 MB at
  batch 50 vs 696 MB at batch 500).
* **Practical setting:** `workers: 16–32`, `batchSize: 100`. At 32 workers it reaches
  ~90% of the best throughput measured (16 workers: ~80–85%) with under 1 GB of heap.
* **HANA connections:** xpro-oss does not cap its pool, so it opens about one
  connection per worker. HANA's `maximum_connections` is 65,536, so even 128
  workers is ~0.2% of the limit.

### 300,000-file estimate

Straight division from the measured rates (best = plateau, safe = 16 workers,
batch 100):

| Files | Best | Safe |
|---|---|---|
| small (~19 KB, ~5.8 GB) | ~35 s | ~43 s |
| large (~60 KB, ~17.9 GB) | ~77 s | ~92 s |

Treat these as a floor. They assume local, cached files and a local database,
and the runs were 6–20K files. A remote DB, cold or network storage, or a much
larger table will be slower; plan on 1.5–2x and confirm with a larger run.

## PostgreSQL — 2026-10-14

Same host, corpus, tool and method as the HANA runs above; HANA was stopped while
these ran. Raw output:
[small](benchmarks/2026-10-14-postgres-small-files.md),
[large](benchmarks/2026-10-14-postgres-large-files.md).

### Environment differences from the HANA runs

| | HANA run | PostgreSQL run |
|---|---|---|
| Database | HANA express 2.00.088 | PostgreSQL 16.15 (Alpine/musl), existing `test-postgres` container, **default settings** (`shared_buffers` 128 MB, `max_connections` 100, `fsync` and `synchronous_commit` on) |
| Docker daemon | root, normal bridge networking | admin's **rootless** daemon, host port `15432` forwarded through rootlesskit (extra overhead on every round trip) |
| Table | `NVARCHAR` columns + `NCLOB` | `TEXT` columns, in a scratch database `xpro_bench` (role `xpro_bench`); application databases untouched |
| Worker counts | 1–128 | 1–64 (`max_connections` is 100) |

### Small files — 20,000 files, avg 19.3 KB

Files/s; peak Go heap MB in parentheses. `*` = rows failed, see below.

| workers | batch 50 | batch 100 | batch 500 |
|---:|---:|---:|---:|
| 1 | 823 (22) | 840 (14) | 898 (46) |
| 2 | 1,340 (20) | 1,400 (22) | 1,685 (86) |
| 4 | 2,609 (21) | 2,864 (37) | 3,005 (168) |
| 8 | 3,878 (39) | 4,317 (72) | 4,920 (334) |
| 16 | 5,000 (106) | 5,486 (159) | 5,817 (707) |
| 32 | 5,808 (239) | 5,983 (332) | 6,012 * (1,333) |
| 64 | **6,576** (365) | 6,274 (643) | 6,741 * (1,718) |

### Large files — 6,000 files, avg 59.6 KB

| workers | batch 50 | batch 100 | batch 500 |
|---:|---:|---:|---:|
| 1 | 268 (18) | 278 (32) | 322 (134) |
| 2 | 494 (32) | 522 (59) | 626 (266) |
| 4 | 942 (60) | 1,037 (110) | 1,167 (531) |
| 8 | 1,612 (119) | 1,671 (223) | 1,623 (1,050) |
| 16 | 2,230 (269) | 2,324 (474) | 2,590 (1,585) |
| 32 | 2,408 (535) | 2,555 (948) | 2,611 (1,556) |
| 64 | 2,552 (947) | 2,595 (1,733) | **2,618** (1,584) |

### Failed rows at high concurrency (PostgreSQL only)

At **batch 500 with 32 and 64 workers on small files**, rows failed to insert
(7,596 and 19,442 failed rows, summed over the 3 runs of each combination; every
other combination had none). The Postgres log shows `ERROR: out of shared
memory`, i.e. the lock table ran out: xpro-oss isolates each row with its own
savepoint, and hundreds of savepoints across dozens of concurrent transactions
exceed the default `max_locks_per_transaction`. Throughput at those two points
is therefore not comparable (failed rows skip work) and the best *clean*
small-file figure is 6,576 files/s. Mitigations: smaller batches (≤ 100 never
failed here) or a higher `max_locks_per_transaction` on the server. HANA showed no
failures; it has no savepoints and rolls back per statement.

### HANA vs PostgreSQL (best clean result for each)

| | HANA | PostgreSQL | PostgreSQL / HANA |
|---|---:|---:|---:|
| Small files, best | 8,619 files/s (128 w, batch 500) | 6,576 files/s (64 w, batch 50) | ~76% |
| Large files, best | 3,883 files/s (64 w, batch 100) | 2,618 files/s (64 w, batch 500) | ~67% |
| Small, 16 workers, batch 100 | 7,008 | 5,486 | ~78% |
| Large, 16 workers, batch 100 | 3,249 | 2,324 | ~72% |
| 1 worker, batch 100 (small) | 940 | 840 | ~89% |

* Single-worker throughput is close (~10% apart), so the gap opens with
  concurrency.
* HANA flattened by ~16–32 workers; PostgreSQL was **still rising at 64** (up to
  its 100-connection cap), so it might gain from a larger `max_connections`
  (and `max_locks_per_transaction`).
* **Do not read this as HANA being inherently faster.** The Postgres run used
  default tuning, rootless-Docker networking and a lock-limited configuration;
  HANA ran on the root daemon with the image's own tuning. A fair comparison needs
  the same networking and a tuned Postgres.

### 300,000-file estimate (PostgreSQL)

| Files | Best clean | Safe (16 workers, batch 100) |
|---|---|---|
| small (~19 KB) | ~46 s | ~55 s |
| large (~60 KB) | ~115 s | ~129 s |

Same caveats as the HANA estimate: treat as a floor and plan on 1.5–2x.

## Oracle — 2026-10-14

Same host, corpus, tool and method; HANA and PostgreSQL were stopped. Raw output:
[small](benchmarks/2026-10-14-oracle-small-files.md),
[large](benchmarks/2026-10-14-oracle-large-files.md).

### Environment differences

| | |
|---|---|
| Database | Oracle Database Free 23ai (`gvenzl/oracle-free:23-slim-faststart`), root Docker daemon, normal bridge networking, named volume |
| Connection | service `FREEPDB1`, user `xpro` (created by the image via `APP_USER`) |
| Free-edition limits | `cpu_count` = 2, SGA ~1.5 GB, `processes` = 200 — Oracle Free caps CPU and memory, which bounds how fast it can ingest |
| Table | `VARCHAR2` columns + `CLOB` (`db/migrations/001_init_oracle.sql` shape) |
| Worker counts | 1–64 |

### Small files — 20,000 files, avg 19.3 KB

Files/s; peak Go heap MB in parentheses. `*` = batches failed, see below.

| workers | batch 50 | batch 100 | batch 500 |
|---:|---:|---:|---:|
| 1 | 746 (22) | 787 (14) | 921 (46) |
| 2 | 1,284 (18) | 1,451 (23) | 1,631 (88) |
| 4 | 2,315 (22) | 2,735 (38) | 2,957 (171) |
| 8 | 3,831 (41) | 4,458 (73) | 4,709 (341) |
| 16 | 5,035 * (101) | 4,984 (172) | **5,634** (696) |
| 32 | 6,567 * (242) | 4,202 * (371) | 4,161 (1,404) |
| 64 | 6,165 * (398) | 3,736 * (647) | 4,053 (1,768) |

### Large files — 6,000 files, avg 59.6 KB

| workers | batch 50 | batch 100 | batch 500 |
|---:|---:|---:|---:|
| 1 | 317 (19) | 357 (33) | 394 (135) |
| 2 | 562 (34) | 632 (59) | 787 (267) |
| 4 | 1,072 (59) | 1,231 (110) | 1,373 (538) |
| 8 | 1,806 (119) | 2,054 (224) | 1,856 (1,064) |
| 16 | 2,439 (309) | 2,621 (460) | 2,885 (1,613) |
| 32 | 2,615 * (577) | 2,830 (931) | **2,969** (1,607) |
| 64 | 2,679 * (1,072) | 2,836 (1,734) | 2,966 (1,599) |

### Failed batches at high concurrency (Oracle only)

Several high-worker combinations lost rows (small files: batch 50 at 16/32/64
workers, batch 100 at 32/64; large files: batch 50 at 32/64; counts are summed
over the 3 runs). Reproducing one case with `bench -v` showed every failure is
the same error, raised when a worker opens its connection:

```
oracle: begin: ORA-12516: TNS:listener could not find available handler with matching protocol stack
```

The listener/instance refuses new sessions while many connections are opened at
once (Oracle Free: `processes` = 200, 2 CPUs). Those throughput numbers are not
comparable (failed batches skip work), so "best" figures below only use
combinations with zero failures. Note that the numbers above were measured before batch-level
failures were added to the result's `errors` list; the current `/process` result
lists them (first 100) next to the `failed` count, and the logs still carry them. Mitigations:
fewer workers (<= 16 here), bigger batches (fewer connection attempts), or a
larger `processes` setting on a non-Free Oracle.

Throughput also *drops* above 16 workers for small files at batch 500
(4,161 at 32 workers vs 5,634 at 16): the database is limited to 2 CPU threads,
so extra concurrency only adds contention.

### Three-way comparison (best result with zero failed rows)

| | HANA | PostgreSQL | Oracle Free |
|---|---:|---:|---:|
| Small files, best files/s | 8,619 | 6,576 | 5,634 |
| …configuration | 128 w, batch 500 | 64 w, batch 50 | 16 w, batch 500 |
| Large files, best files/s | 3,883 | 2,618 | 2,969 |
| …configuration | 64 w, batch 100 | 64 w, batch 500 | 32 w, batch 500 |
| Small, 16 w, batch 100 | 7,008 | 5,486 | 4,984 |
| Large, 16 w, batch 100 | 3,249 | 2,324 | 2,621 |
| Small, 1 w, batch 100 | 940 | 840 | 787 |
| Large, 1 w, batch 100 | 404 | 278 | 357 |

* With a single worker the three are within ~30% of each other (~16% on small files); they diverge with
  concurrency.
* Large files: Oracle beat the default-tuned, rootless-networked PostgreSQL run
  and trailed HANA by ~24%. Small files: Oracle was the slowest.
* These are three different set-ups (see each section's environment table), not a
  vendor benchmark: HANA express and Oracle Free are capped editions on a shared
  16-core host, and PostgreSQL ran with default settings behind rootless Docker.

### 300,000-file estimate (Oracle Free)

| Files | Best clean | Safe (16 workers, batch 100) |
|---|---|---|
| small (~19 KB) | ~53 s | ~60 s |
| large (~60 KB) | ~101 s | ~114 s |

Same caveats as the other estimates: a floor; plan on 1.5–2x.

## Reproducing

Prerequisites: a reachable target (HANA here), a `config.yaml` with the target
and mapping, and a **scratch table** — the tool truncates it before every run.

```bash
# 1. create the scratch table (same DDL as the real one, different name)
./migrate scratch_table.sql

# 2. generate a corpus: N copies of a template with a unique value in one tag
go run ./cmd/bench -gen ./corpus/small -template sample.xml -count 20000 -tag OBJECT_ID

# 3. sweep workers x batch size, 3 runs each (median); output is Markdown
go run ./cmd/bench -config config.bench.yaml -dir ./corpus/small \
    -workers 1,2,4,8,16,32,64,128 -batches 50,100,500 -repeat 3 \
    -truncate "TRUNCATE TABLE bench_records" > docs/benchmarks/$(date +%F)-small-files.md
```

`config.bench.yaml` is `config.yaml` with `target.table` pointed at the scratch
table. Source `path` in the config is ignored in favor of `-dir`.

## Limitations

* One host, one table per database; HANA and PostgreSQL each ran in Docker on the same VM,
  with different Docker daemons and networking (see the PostgreSQL section).
* Peak heap is sampled every 20 ms from the Go runtime, so it is approximate and
  excludes HANA's memory.
* The bench does not report live DB connection counts. The ~11 connections
  observed for 16 workers came from sampling `M_CONNECTIONS` during a separate
  run.
* Only a local-directory source was measured. SFTP / BTP-tunneled sources were not.
* Only the first XPath match per file is stored in scalar columns (a file with
  several bills inserts one row).
