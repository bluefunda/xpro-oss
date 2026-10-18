# AGENTS.md

This is `xpro-oss` — a generic, config-driven XML ingestion pipeline. It's a
from-scratch, minimal service that generalizes the batched
INSERT/savepoint-isolation pattern proven in production at BlueFunda so it isn't
tied to any particular XML schema or SAP concept.

Module: `github.com/bluefunda/xpro-oss` | Go 1.26 (see `go.mod` for the exact patch)

## Purpose

Read files from a source (local directory, or SFTP — direct or tunneled
through SAP BTP Connectivity), map configured XPath expressions onto target
columns, batch-insert into PostgreSQL (default), Oracle or SAP HANA — all pure Go
drivers, no CGO. Oracle and HANA targets can also be resolved via a named SAP BTP
destination instead of direct connection fields.
Everything is driven by `config.yaml` — there is no hardcoded schema.

## Build & Verify

```bash
go build ./...                        # default build (PostgreSQL only, pure Go)
go vet ./...
go test -race ./...
gofmt -l .                            # must be empty
golangci-lint run ./...               # org CI runs this with the default config
govulncheck ./...                     # must report 0 affecting vulnerabilities
```

## Key packages

| Package | Purpose |
|---|---|
| `config` | YAML config (strict keys), `${VAR}` expansion after parsing; plain data, no network I/O |
| `btp` | Optional SAP BTP binding detection (`Detect`) and Destination lookup (`Bindings.Resolve`); a nil `*Bindings` is valid |
| `model` | Generic, schema-less `Record` shape |
| `parser` | Config-driven XPath → column mapping |
| `source` | `SFTP` (direct or BTP-tunneled, optional `known_hosts` verification); local dirs use `os.DirFS` |
| `db` | One `Inserter` + `Dialect` (Postgres default, Oracle, HANA) over per-vendor `driver.Connector`s — per-row-isolated batched inserts, all pure Go |
| `process` | Pipeline orchestration: batching, context-aware bounded worker pool, inline retry of transient failures |
| `handler` | HTTP handlers: `/healthz`, `/readyz`, `/process` (async, single-flight), `/status` |
| `cmd/migrate` | Applies a raw SQL file to the configured target |
| `cmd/bench` | In-process throughput benchmark: corpus generator + workers × batch sweep → Markdown |
| `examples/` | Docker Compose stacks (postgres, oracle, hana), `config.yaml`, `schema.sql`, sample XML |
| `docs/` | `RUNNING.md` (turn-key guide), `BENCHMARKS.md` (+ raw `docs/benchmarks/`), `HANA_DOCKER.md` |
| `licenses/` | Full text of third-party licenses that must ship with the binary (xmlpath LGPL-3.0) |

## Dependencies

Pure Go only — no CGO, no C toolchain, no DB client libraries. Prefer the
standard library; third-party modules must come from SAP, the Go team, or be
an established community pure-Go driver. Do not add a dependency without
updating this table and the README's.

| Module | Role | Origin |
|---|---|---|
| `github.com/SAP/go-hdb` | SAP HANA driver (`driver`, `driver/dial`, `sqlscript`) | SAP |
| `github.com/lib/pq` | PostgreSQL driver | community |
| `github.com/sijms/go-ora/v2` | Oracle driver | community |
| `github.com/pkg/sftp` | SFTP client | community |
| `golang.org/x/crypto` (+ `x/net`, `x/sys`, `x/text`) | SSH, and transitive deps of go-hdb | Go team |
| `gopkg.in/xmlpath.v2` | XPath evaluation | Canonical |
| `gopkg.in/yaml.v3` | config parsing | community |
| `github.com/bluefunda/btp-go/*` | BTP binding, destination, connectivity, sshclient, xsuaa | bluefunda |

Org baselines: Go version is whatever `go.mod` says (currently 1.26, bumped by
`go-hdb`); CI uses `go-version-file: go.mod`; the Dockerfile builds with
`golang:1.26-alpine` + `GOTOOLCHAIN=local` and ships on
`gcr.io/distroless/static:nonroot`, published to `ghcr.io/bluefunda/xpro-oss`.
When `go.mod`'s Go version changes, update the Dockerfile's `GO_VERSION` default.

## Per-row isolation (SAVEPOINT)

`db.Inserter.InsertBatch` (db/db.go) is the single shared batch-INSERT path:
one transaction, one prepared statement, per-row failure isolation. What differs
per database is a `db.Dialect` value (placeholder style, batch size, savepoint
support, optional script splitting) plus a `driver.Connector` built in
`postgres.go` / `oracle.go` / `hana.go` — there is no per-database insert code.

- **Postgres, Oracle** (`Dialect.Savepoints=true`): `SAVEPOINT sp_N` before each
  row, `ROLLBACK TO SAVEPOINT sp_N` when that row's INSERT fails.
- **HANA** (`Savepoints=false`): HANA has no `SAVEPOINT` statement, but rolls a
  failed statement back on its own and keeps the transaction usable — so the
  loop just records the failure and continues. Never emit SAVEPOINT SQL for HANA.
- Placeholders differ per driver: `$n` (Postgres), `:n` (Oracle), `?` (HANA).
- `process.Pipeline` retries a batch once, and only when the whole transaction
  failed (`Success==0`, every record failed) **and** `db.Retryable(err)` says the
  error is transient (dropped/refused connection, EOF, network timeout). It never
  retries partial failures (duplicate-key risk), server-side errors, or a failed
  commit (`db.ErrCommitUncertain`: the server may have applied it — a retry could
  insert the batch twice).
- A new database = a new `Dialect` var + a `…Connector` func + a case in
  `db.Open`; add tests using the fake `database/sql` driver in
  `db/fakedriver_test.go`. Do not reintroduce per-driver struct types.

## Design rules (from the Go-idiom review)

- Constructors return ready objects (`db.Open` pings and returns `*db.Inserter`);
  no `New` + `Open` two-phase init.
- Consumers define the small interfaces they need (`process.Inserter`); producers
  return concrete types. `handler` depends on `process.Inserter`, not on `db`.
- The creator of a resource owns its lifecycle: `handler.SourceFactory` connects
  and closes sources; `process.Pipeline` only reads an `fs.FS`.
- `POST /process` is single-flight (409 on overlap) and runs under the handler's
  base context (cancelled on SIGTERM), never under the request's. Keep it that
  way: overlapping runs insert every file twice.
- Every file is accounted for in `process.Result` (`Inserted + Failed == Files`);
  never drop a read/parse failure on the floor. `Result.Errors` is capped.
- `config` stays plain data; env expansion happens after YAML parsing, unknown
  keys are rejected, and `MappingField.Const` is a `*string` so an empty const is
  not mistaken for "store the raw file".
- Libraries log through an injectable `*slog.Logger` (nil = `slog.Default()`);
  don't mutate the global default outside `main`.

## Testing

Every package has tests; none need a live database or network. `db` tests use
an in-memory `database/sql` driver (`db/fakedriver_test.go`), `source` tests
run an in-process SSH/SFTP server (including `known_hosts` verification), `btp`
tests fake the BTP token and Destination endpoints with `httptest`. `main` and
`cmd/migrate` keep their logic in testable `run()` functions; only the signal
wiring and the happy-path server loop (which need a live database) are untested.

## Docs, examples and benchmarks

- `README.md` is the entry point and links everything below; keep it in sync when
  you add a doc, example or package.
- `docs/RUNNING.md` is the user guide for the container image and Compose stacks.
  If you change config fields, endpoints, the image layout or example behavior,
  update it and the matching `examples/<db>/` files in the same PR.
- `examples/<db>/compose.yaml` stacks are **manually** tested (they need Docker and
  the database images): `docker compose up -d`, `POST /process`, read the rows back,
  run `up -d` a second time, `down -v`. Do this for all three after changing the
  Dockerfile, `/migrate`, or a driver.
- `docs/BENCHMARKS.md` holds the numbers quoted in the README. Re-run
  `go run ./cmd/bench` (see the doc) and update both when a change could affect
  ingest throughput (pipeline, drivers, batching); keep raw output in
  `docs/benchmarks/`. Benchmarks use a scratch table — the tool truncates it.
- A new dependency or a version bump of a bundled module must also update
  `THIRD_PARTY_LICENSES` (and `licenses/` if its license requires shipping the text).

## Releasing and the container image

- Version and `CHANGELOG.md` come from Release Please; use Conventional Commits.
  To force a specific version (e.g. the first release), put a `Release-As: 0.0.1`
  footer in a commit message.
- `Dockerfile`: builder stage is `--platform=$BUILDPLATFORM` and cross-compiles
  with `GOOS/GOARCH=$TARGETOS/$TARGETARCH` (no QEMU needed; the org
  `docker-deploy.yml` workflow does not set it up). The image runs from `WORKDIR /`
  (distroless starts in `/home/nonroot`, where `config.yaml` is not), ships `/xpro-oss`,
  `/migrate`, a default `/config.yaml` and `/licenses/`.
- `GO_VERSION` is injected from the `go` line in `go.mod`, so that line must be an
  existing `golang:<version>-alpine` tag; keep it on a patched release (check
  `govulncheck ./...`).
- Before a release: `go vet ./...`, `gofmt -l .`, `go test -race ./...`,
  `golangci-lint run ./...` (the org CI lint job; default config), `govulncheck ./...`,
  build the image (amd64 + arm64 with `docker buildx build --platform
  linux/amd64,linux/arm64`), scan it (e.g. Trivy) and run the three Compose stacks.
- Pushing to `main` only opens/updates the release PR; the image is published when
  that PR is merged and the `deploy` job runs.
- `/process` and `/status` are unauthenticated by design (see `handler.RegisterRoutes`);
  never expose them without a proxy or middleware.


## Pipeline (pre-wired to org-wide reusable workflows)

```
PR opened     → go-review.yml        advisory Claude Code review comment
PR opened     → pr-title-check.yml   enforce conventional commit title
PR + main     → ci.yml               build, test (race), vet

Push to main  → release-please.yml   version bump + CHANGELOG + release PR
On release    → release-please.yml   chains github-release-notes automatically
```

See [`bluefunda/.github`](https://github.com/bluefunda/.github) for the full
workflow definitions.

## Go style

- Standard library patterns preferred: accept interfaces, return concrete types.
- `context.Context` as the first argument on all IO-bound functions.
- Errors wrapped with `%w`.
- No hardcoded XML schema, table name, or SAP-specific concept outside of
  `config.yaml` and the `Source`/`Target` interface implementations — that's
  the whole point of this repo.

## Conventions

- Conventional Commits for all commit messages and PR titles (`feat:`,
  `fix:`, `chore:`, `docs:`, `test:`, `refactor:`).
- Release Please manages versioning and `CHANGELOG.md` — do not manually
  edit it.
- `go vet ./...` and `go test -race ./...` must pass before merging.
- Keep `source` and `db` generic — vendor- or schema-specific behavior
  belongs in `config.yaml`, never hardcoded in Go.
