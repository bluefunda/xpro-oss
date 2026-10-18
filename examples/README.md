# Examples

Turn-key Docker Compose stacks for running xpro-oss against each supported database.
Each stack is `database` → one-shot schema `migrate` → `xpro-oss`, with a ready
`config.yaml`, `schema.sql` and `.env.example`. Full guide:
[docs/RUNNING.md](../docs/RUNNING.md).

| Directory | Database | Start with |
|---|---|---|
| [`postgres/`](postgres) | PostgreSQL 16 | `cd postgres && cp .env.example .env && docker compose up -d` |
| [`oracle/`](oracle) | Oracle Database Free 23ai (`gvenzl/oracle-free`) | same; first start 1–2 min |
| [`hana/`](hana) | SAP HANA, express edition (`saplabs/hanaexpress`) | same, after reading the header of `hana/compose.yaml` (rootful Docker, >= 16 GB RAM, host sysctls, SAP license); first start ~3–5 min |
| [`data/`](data) | sample input | two tiny XML files, mounted read-only into every stack |

```bash
cd examples/postgres
cp .env.example .env            # set DB_PASSWORD
docker compose up -d
curl -X POST "localhost:8080/process?wait=true"   # {"files":2,"inserted":2,"failed":0,...}
curl localhost:8080/status
docker compose down -v                     # remove containers and data
```

Notes
* The images come from `ghcr.io/bluefunda/xpro-oss:latest`; set `XPRO_IMAGE` in `.env`
  to use another tag or a locally built image (`docker build -t xpro-oss:dev .`).
* The published port is bound to `127.0.0.1` because `/process` and `/status` have no
  authentication. Change `XPRO_PORT` if 8080 is taken.
* Without `?wait=true`, `POST /process` returns `202` immediately and the run continues in the
  background (poll `/status`); only one run happens at a time (a second request gets `409`).
* Every `POST /process` reads **all** files in the input folder again; xpro-oss does not
  move or de-duplicate them.
* The database images carry their own licenses (PostgreSQL License, Oracle Free use
  terms, SAP HANA express license) — running them means accepting those terms.
* Performance numbers for these databases: [docs/BENCHMARKS.md](../docs/BENCHMARKS.md).
