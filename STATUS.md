# Gline Implementation Status

Updated: 2026-08-27

## Current Stable Boundary

- Checkpoint `6c5b1e6` contains the existing Agent lifecycle prototype, initial
  Gin upload handler, tests, examples, and architecture/tutorial documentation.
- Baseline verified with `go build ./cmd/...`, `go test ./... -count=1`,
  `go test -race ./... -count=1`, and `go vet ./...`.
- Branch `codex/full-gline` is isolated in `E:\Proj\gline-full`.

## Current Candidate

- The current branch HEAD contains the candidate described below.
- The modular Server now contains authenticated Control, Ingest, Query and
  Operations planes with PostgreSQL repositories and embedded migrations.
- Project creation, its default 14-day retention policy and audit event share
  one transaction. Project disable also pauses active Pipelines atomically.
- Ingest isolates tenants, commits Batch, Entries and Usage together, and uses
  `(project_id, batch_id)` plus a canonical payload hash for retry semantics.
- The reliable Agent persists file checkpoints and batches in a bounded CRC WAL,
  recovers truncated tails, survives Windows rename/recreate and copy-truncate
  rotation, retries temporary delivery failures and reports heartbeats. A 2xx
  response only releases a batch when its ACK contains the exact same batch ID.
- Immutable per-batch failures are durably moved to Agent-local quarantine so
  later batches can proceed; operators can list metadata or explicitly discard
  one batch while the Agent is stopped. Authentication and endpoint failures
  stop the Agent with the batch still pending.
- API-key last-used observation is bounded and best-effort, so an auxiliary
  write failure cannot reject valid authentication. Queries have a configurable
  server-side execution deadline in addition to their range/page limits.
- Server metrics cover HTTP route templates, ingest, query, database pool and
  bounded background jobs without Project, Batch, request or raw-error labels.
  The reliable Agent optionally exposes loopback-only WAL, delivery, parse and
  Pipeline metrics computed from recovered state.
- Liveness is independent of PostgreSQL. Readiness checks PostgreSQL and turns
  unavailable as soon as graceful shutdown enters draining state.
- GitHub Actions separates formatting/vet/unit/Linux-build, race, real
  PostgreSQL integration and Web lint/test/build jobs with read-only checkout
  permissions.
- The React console implements bootstrap login, Project/Key/Agent/Pipeline
  control, log search, retention, usage, audit, quarantine and health views.
- Dockerfiles, Compose topology, environment template, reliable Agent example
  and root runbook are present.

## Verification Evidence

- `go build ./cmd/...`: passed.
- `GOOS=linux CGO_ENABLED=0 go build ./cmd/...`: passed.
- `go test ./... -count=1`: passed.
- `go test -race ./... -count=1`: passed.
- `go vet ./...`: passed.
- `pnpm lint`, `pnpm test`, `pnpm build`: passed; lint has zero warnings and
  four frontend tests pass.
- `docker compose config --quiet`: passed with non-secret validation values.
- Vite dev runtime: `HTTP 200` at `http://127.0.0.1:5173/`.
- Focused reliability coverage includes mismatched ACK rejection, durable local
  quarantine and discard, continuation after a bad batch, systemic stop with a
  pending batch, and offline rename/recreate epoch transition.
- The tagged PostgreSQL suite builds, including a full HTTP -> authentication ->
  Control -> Ingest -> Query -> PostgreSQL workflow. Both database-backed cases
  explicitly report `SKIP` locally because `GLINE_TEST_DATABASE_URL` is unset;
  CI is configured to run them against PostgreSQL 17.
- Secret and local absolute module replacement review: no committed credential
  found; the obsolete `E:/Proj/testx` replacement was removed.
- The GitHub Actions workflow was locally reviewed and its commands were run in
  their equivalent local forms; it has not yet executed on GitHub.

## Remaining Runtime Gate

- The opt-in PostgreSQL integration test and full Compose E2E have not run.
- Docker Desktop 4.49.0 crashes before starting its engine because it cannot
  remove `C:\Users\Yu\AppData\Local\Docker\run\dockerInference`, a stale
  reparse-point socket. No Docker reset, volume deletion or socket deletion was
  performed.
- Until PostgreSQL is available, the Server, Agent-to-Server ingest and complete
  browser workflow are implemented and statically verified but not marked
  runnable or accepted.

## Evidence Levels

| Area | Implemented | Verified | Integrated | Runnable | Accepted |
| --- | --- | --- | --- | --- | --- |
| Reliable Agent | yes | yes | protocol-level | blocked by Server runtime | no |
| Control plane | yes | yes | yes | blocked by PostgreSQL | no |
| PostgreSQL ingest/query | yes | unit/race/tag-build | code-level | blocked by Docker | no |
| Operations and maintenance | yes | yes | yes | blocked by PostgreSQL | no |
| Web application | yes | yes | API contract | standalone dev server | no |
| Compose environment | yes | config only | topology | Docker engine blocked | no |

Server-side Quarantine tables, lease recovery and replay APIs are implemented
but intentionally have no producer in the synchronous ingest path. Agent-local
quarantine is the active failure path; a future asynchronous processor may use
the Server-side lifecycle only after it can durably store payloads before ACK.
