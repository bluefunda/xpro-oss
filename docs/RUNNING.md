# Running xpro-oss with Docker

A turn-key guide: pull the image, point it at a database, drop XML files in a
folder, and ingest them. Ready-made Docker Compose stacks for **PostgreSQL**,
**Oracle** and **SAP HANA** live in [`examples/`](../examples).

Image: `ghcr.io/bluefunda/xpro-oss` (tags `latest` and release versions such as
`v0.0.1`, published by the org's release workflow). It is a ~66 MB distroless image
that runs as a non-root user (uid 65532) and contains:

| Path | What |
|---|---|
| `/xpro-oss` | the daemon (`ENTRYPOINT`); reads `/config.yaml`, listens on `:8080` (override with `PORT`) |
| `/migrate` | helper that applies a SQL file to the configured target (`--entrypoint /migrate`) |
| `/config.yaml` | a default config — mount your own over it |
| `/licenses/` | `LICENSE` (Apache-2.0), `THIRD_PARTY_LICENSES` and the xmlpath LGPL text |

## How it works (read this once)

* xpro-oss is a small HTTP service. **Nothing happens until you call**
  `POST /process`: it lists every file in `source.path`, maps XPath expressions
  from `config.yaml` onto table columns, and batch-inserts them.
* **It does not move, delete or de-duplicate files.** Calling `/process` twice on
  the same folder inserts every file twice. Use an inbox folder you empty after a
  successful run, call it once per new batch, or give the table a unique key.
* `/process` and `/status` have **no authentication**. The example stacks publish
  the port on `127.0.0.1` only; put a reverse proxy or network policy in front if
  you expose it.
* Endpoints: `GET /healthz` (liveness), `GET /readyz` (readiness: the database
  answers a ping), `POST /process[?limit=N][&wait=true]`, `GET /status` (whether a
  run is in progress, and the result of the last finished run).
* **One run at a time.** `POST /process` returns `202` and runs in the background;
  poll `/status`. A second request while a run is in progress gets `409`. Add
  `?wait=true` to block until the run finishes and get its result in the response
  (a client that disconnects does not cancel the run). Runs stop after 30 minutes,
  and on SIGTERM (the partial result stays in `/status`).
* **Every file is accounted for:** `inserted + failed == files`. Unreadable or
  unparseable files and failed batches appear in `errors` (first 100; `errorsOmitted`
  counts the rest).
* **SFTP host keys are not verified unless you set `source.sftp.knownHosts`** to an
  OpenSSH `known_hosts` file; a warning is logged on every connect otherwise.

## Quick start (Docker Compose)

You need Docker with the Compose plugin and the example files (clone this repo or
copy the `examples/` directory).

```bash
cd examples/postgres        # or examples/oracle, examples/hana
cp .env.example .env        # then edit .env and set real passwords
docker compose up -d        # database -> create table -> xpro-oss, in that order
```

Put your XML files in `examples/data/` (two samples are included), then:

```bash
curl -X POST "localhost:8080/process?wait=true"   # -> {"files":2,"inserted":2,"failed":0,...}
curl localhost:8080/status
docker compose logs -f xpro-oss
```

Change the port with `XPRO_PORT=18080` in `.env`, or pin another image with
`XPRO_IMAGE=ghcr.io/bluefunda/xpro-oss:v0.0.1`.

What each stack does:

| Stack | Database image | Notes |
|---|---|---|
| `examples/postgres` | `postgres:16-alpine` | simplest; ready in seconds |
| `examples/oracle` | `gvenzl/oracle-free:23-slim-faststart` (Oracle Database Free 23ai) | first start 1–2 min; Free edition is limited to 2 CPU threads / 2 GB RAM |
| `examples/hana` | `saplabs/hanaexpress` | needs a rootful Docker daemon, >= 16 GB free RAM, host kernel settings and acceptance of SAP's license — read the header of `examples/hana/compose.yaml` and [HANA_DOCKER.md](HANA_DOCKER.md); first start ~3–5 min |

Each stack has three services: `db`, a one-shot `migrate` (runs `schema.sql`) and
`xpro-oss`, which starts after `migrate`. `schema.sql` is safe to re-run on PostgreSQL
and Oracle; on HANA a re-run exits 1 (table exists) without affecting xpro-oss.
`restart: unless-stopped` keeps the database and xpro-oss up across reboots.

Stop / remove:

```bash
docker compose stop            # keep data
docker compose down            # remove containers, keep the database volume
docker compose down -v         # remove everything, including the data volume
```

## Using your own database

Skip the `db` service: copy a `config.yaml` from `examples/<db>/`, point
`target.host/port/user/database` at your server, create the table (adapt
`schema.sql` to your `mapping`), and run only the daemon:

```bash
docker run -d --name xpro-oss --restart unless-stopped \
  -p 127.0.0.1:8080:8080 \
  -e DB_PASSWORD='...' \
  -v "$PWD/config.yaml:/config.yaml:ro" \
  -v /srv/xml-inbox:/data/in:ro \
  ghcr.io/bluefunda/xpro-oss:latest

# create the table once
docker run --rm -e DB_PASSWORD='...' \
  -v "$PWD/config.yaml:/config.yaml:ro" -v "$PWD/schema.sql:/schema.sql:ro" \
  --entrypoint /migrate ghcr.io/bluefunda/xpro-oss:latest /schema.sql
```

The XML folder must be readable by uid 65532 (e.g. `chmod -R a+rX /srv/xml-inbox`).
`${VAR}` references anywhere in `config.yaml` are expanded from the container
environment, so keep secrets out of the file.

## Configuration in 60 seconds

```yaml
source:
  type: local
  path: "/data/in"            # or type: sftp (see README)
target:
  db: postgres                # postgres | oracle | hana
  table: "public.records"
  batchSize: 100              # rows per transaction
  host: db
  port: 5432
  user: xpro
  password: "${DB_PASSWORD}"
  database: xpro
workers: 8                    # concurrent batches
mapping:                      # drives both the XPath extraction and the INSERT columns
  - column: account_number
    xpath: "/Root/Account/Number"
  - column: raw_xml
    xpath: ""                 # empty = store the whole file
```

Per-database fields (service name, TLS mode, schema) are commented in each
`examples/<db>/config.yaml`; the full reference is in the [README](../README.md).
Only the **first** XPath match per file goes into a column.

## Triggering runs

xpro-oss is a service, not a watcher. Typical triggers:

```bash
# cron: every 5 minutes (check "failed" in the response before clearing the inbox)
*/5 * * * * curl -fsS -X POST "localhost:8080/process?wait=true" >> /var/log/xpro-run.log

# Kubernetes CronJob / systemd timer: the same curl command
```

Because runs are not de-duplicated (see above), pair the trigger with whatever
moves processed files out of `source.path`.

## Tuning

Measured with a local-directory source (details and raw data in
[BENCHMARKS.md](BENCHMARKS.md)): throughput grows steadily up to ~16
workers (about 7x over a single worker) and flattens after ~16–32. A good default is `workers: 16`,
`batchSize: 100`. Larger batches use much more memory for little gain, and on
PostgreSQL and Oracle very high concurrency can fail (see Troubleshooting).

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `read config.yaml: no such file or directory` | You overrode `WORKDIR`/entrypoint; the binary reads `./config.yaml` and the image runs from `/`. Mount your file at `/config.yaml`. |
| `failed` > 0 in a run result but no error text | Batch-level failures (connection refused, commit error) are logged, not listed in the result. Check `docker compose logs xpro-oss`. |
| Oracle `ORA-12516 ... could not find available handler` | Too many sessions opened at once (Oracle Free: 200 processes, 2 CPUs). Lower `workers` (<= 16) or raise `processes`. |
| Postgres `out of shared memory` with large batches | One savepoint per row exhausts the lock table. Use `batchSize <= 100` or raise `max_locks_per_transaction`. |
| HANA container restarts, "Check failed: syscalls" | That warning is harmless. The real cause is in the lines after it; common ones: not enough free RAM, host kernel parameters missing (`examples/hana/hana-sysctl.conf`), rootless Docker. |
| HANA stack: `migrate` shows `Exited (1)` / `duplicate table name` after the first start | Expected: HANA has no `CREATE TABLE IF NOT EXISTS`, so re-running the one-shot fails harmlessly while xpro-oss keeps running. |
| HANA `password` errors | Master password needs >= 8 chars, upper + lower + digit, no ``` ` $ \ ' " ```, no dictionary words. |
| `permission denied` reading XML | The inbox must be readable by uid 65532. |
| `pull access denied` for the image | The package may be private or not yet published; `docker login ghcr.io` with a token that has `read:packages`, or build it (below). |

## Licenses

xpro-oss is Apache-2.0; bundled third-party components are listed in
`/licenses/THIRD_PARTY_LICENSES` inside the image. The Compose examples pull
separate database images with their own terms — PostgreSQL (PostgreSQL License),
Oracle Database Free (Oracle's free-use terms) and SAP HANA express (SAP's license,
accepted via `--agree-to-sap-license`). Using them means you accept those terms.

## Build the image yourself

```bash
docker build -t ghcr.io/bluefunda/xpro-oss:dev .
XPRO_IMAGE=ghcr.io/bluefunda/xpro-oss:dev docker compose -f examples/postgres/compose.yaml up -d
```

`GH_PAT` is an optional build arg for private `github.com/bluefunda/*` modules.
