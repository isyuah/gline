# Gline Implementation Status

Updated: 2026-08-28

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
- Ingest admission applies per-key request rate and per-Project entry, byte and
  in-flight budgets before opening a database transaction. 429 responses carry
  retry timing and the Agent keeps the same batch; accepted batches commit
  usage while duplicate/failed transactions refund reservations. Limits are
  explicitly single-instance, not a global multi-replica quota.
- Project query concurrency is fail-fast: a full local semaphore returns 429
  with `Retry-After` instead of waiting until the query deadline and surfacing
  an ambiguous internal error. Database execution has its own bounded deadline
  and stable 504 `query_timeout` contract.
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
- `docker compose up --build -d`: passed on Docker Engine 28.5.1 / Compose
  v2.40.2 after the Web build context was reduced with `web/.dockerignore`.
  PostgreSQL, Server and Web containers are currently running in
  `E:\Proj\gline-full`; this local instance publishes Server on `18080` because
  another user-owned process already occupies `8080`, and publishes Web on
  `4173`.
- Compose runtime smoke: `/livez`, `/readyz`, `/metrics` and the Web home page
  returned HTTP 200. The HTTP workflow created a Project, Agent, Pipeline and
  scoped API key; a batch returned `accepted`, the identical retry returned
  `duplicate`, a filtered query returned one entry, Usage returned one bucket,
  and the low-cardinality ingest/query/admission metric families were present.
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

## Remaining Acceptance Work

- The tagged PostgreSQL suite is still opt-in and has not been pointed at a
  disposable test database in this worktree. The Compose smoke covers the
  production-shaped HTTP-to-PostgreSQL path; CI remains the authoritative
  isolated integration run.
- The browser workflow remains to be manually accepted by the user. Automated
  checks establish that the Web bundle is served and its same-origin API proxy
  is reachable; they do not claim visual or interaction acceptance.

## Evidence Levels

| Area | Implemented | Verified | Integrated | Runnable | Accepted |
| --- | --- | --- | --- | --- | --- |
| Reliable Agent | yes | yes | protocol-level | blocked by Server runtime | no |
| Control plane | yes | yes | yes | yes (Compose) | no |
| PostgreSQL ingest/query | yes | unit/race/tag-build + Compose smoke | yes | yes (Compose) | no |
| Operations and maintenance | yes | yes | yes | yes (Compose) | no |
| Web application | yes | yes | API contract + served bundle | yes (Compose) | no |
| Compose environment | yes | config + image build | yes | yes | no |

Server-side Quarantine tables, lease recovery and replay APIs are implemented
but intentionally have no producer in the synchronous ingest path. Agent-local
quarantine is the active failure path; a future asynchronous processor may use
the Server-side lifecycle only after it can durably store payloads before ACK.
