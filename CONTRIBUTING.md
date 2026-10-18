# Contributing to xpro-oss

Thanks for your interest in contributing! This document covers the
development workflow, coding standards, and how to add a new source or
target driver.

## Getting started

```bash
git clone https://github.com/bluefunda/xpro-oss.git
cd xpro-oss
go mod download
go build ./...
```

Requirements: Go 1.26+ (see `go.mod`). The PostgreSQL, Oracle and SAP HANA
targets are pure Go — no CGO, ever. See the README's Dependencies section for
every module and its origin.

## Development workflow

```bash
go build ./...
go test -race ./...
go vet ./...
gofmt -l .                      # must print nothing
```

## Commit conventions

All commits and PR titles must follow
[Conventional Commits](https://www.conventionalcommits.org/):

```
feat: add MySQL target driver
fix(sftp): retry on transient handshake EOF
chore: bump golang.org/x/crypto
```

Types: `feat`, `fix`, `chore`, `docs`, `refactor`, `perf`, `test`, `ci`,
`style`, `build`, `revert`. Breaking changes: append `!` after the type —
`feat!: change mapping column order semantics`.

## Adding a source or target driver

xpro-oss is deliberately generic — there is no hardcoded XML schema, table,
or vendor concept outside `config.yaml` and the two extension points below.

**A new `source.Source`:**

1. Implement a plain `io/fs.FS` (ideally also `fs.ReadDirFS`) in a new file
   under `source/`.
2. If it needs a session, give it `Connect(ctx) error` and `Close() error`;
   whoever creates it (the `handler.SourceFactory` in `main.go`) connects it and
   returns the close function — `process.Pipeline` only reads from the `fs.FS`.
3. Wire it into `buildSourceFactory` in `main.go`, gated on a new
   `source.type` value in `config.yaml`.

**A new database target:**

1. Add a `db.Dialect` value (placeholder style, recommended batch size,
   `Savepoints` true or false, optional `SplitScript`) and a
   `…Connector(ctx, cfg, resolve) (driver.Connector, error)` function in a new
   file under `db/` — see `db/postgres.go` (savepoint isolation) and
   `db/hana.go` (no SAVEPOINT support: the database rolls a failed statement
   back itself). Batch inserting is shared (`Inserter.InsertBatch`); there is no
   per-database insert code to write.
2. Wire it into `db.Open` in `db/db.go`, gated on a new `target.db` value.
3. Prefer a driver from the database vendor, SAP or the Go team, and a pure-Go
   one if it exists (see `db/postgres.go`, `db/oracle.go`, `db/hana.go`, all CGO-free) so the default build stays pure Go for
   everyone. If your driver genuinely needs CGO or other build-time
   dependencies, gate it behind a build tag with a stub fallback instead of
   forcing it on the default build.

## Pull request checklist

- [ ] `go test -race ./...` passes
- [ ] `go vet ./...` and `gofmt -l .` are clean
- [ ] New functionality has tests
- [ ] PR title follows Conventional Commits
- [ ] README/AGENTS.md updated if behavior changed

## Code style

See [AGENTS.md](AGENTS.md) for the full style guide. Key points:

- Standard library patterns preferred; small, single-method interfaces.
- `context.Context` as the first argument on IO-bound functions.
- Errors wrapped with `%w`.
- Table-driven tests; prefer real implementations (e.g. `httptest`,
  in-memory fakes) over mocking frameworks.

## Releasing

Releases are automated via [Release Please](https://github.com/googleapis/release-please).
Merge a `release-please--*` PR to cut a release — you don't need to manually
tag anything or edit `CHANGELOG.md`.
