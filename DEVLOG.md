# Gline Development Log

## 2026-08-31 - Agent control and recovery hardening

- Added a bidirectional heartbeat contract. The Agent sends observed Pipeline
  status, error summary and local `config_version`; the Server returns desired
  status and the authoritative config version for that Agent.
- Added an Agent-side control gate. `paused` stops new file reads while durable
  batches drain, `disabled` preserves pending data, and a config-version drift
  reports an explicit error instead of silently running stale configuration.
- Isolated Pipeline source failures so one broken file does not cancel the
  entire Agent. Dispatcher delivery now skips temporarily unavailable batches
  so unrelated Pipelines can continue without losing ordering within a batch.
- Changed Server ingest semantics so paused Pipelines accept their already
  durable backlog; disabled/error Pipelines remain hard boundaries. Idempotency
  conflicts now use `idempotency_conflict`, separate from resource state.
- Added a real PostgreSQL-backed integration test with a toggled 503 outage. It
  verified file -> WAL -> recovery -> committed query results and WAL drain.
- Removed unused pre-reliable Server upload/sink prototype packages. Historical
  architecture material is labeled as historical; the backend tutorial and
  root README describe the current boundary.
- Kept the Agent's in-memory sender/terminal path only as an explicitly
  documented development compatibility mode; the reliable WAL path remains the
  supported production and resume-demo path.
- Added the WAL `Commit`/`Ack` fsync benchmark and optimized the normal
  dispatcher queue read to avoid copying all pending payloads every iteration.
- Added a local release script for Windows/Linux Agent/Server binaries with
  trimpath, Server version injection and SHA-256 output; no tag or publication
  was performed.

## 2026-08-28 - Compose runtime acceptance slice

- Verified Docker Engine 28.5.1 and Docker Compose v2.40.2 in the isolated
  `codex/full-gline` worktree.
- Added `web/.dockerignore` so local `node_modules`, build output and logs are
  excluded from the Web image context; switched the Web runtime base to the
  locally available `nginx:1.29-alpine`.
- Started the named-volume Compose stack with PostgreSQL 17, the Go Server and
  the React/Nginx console. Because a separate user-owned service occupies host
  port 8080, the local `.env` publishes Gline Server on 18080 and Web on 4173.
- Verified HTTP liveness, readiness, metrics, Web serving, Project/Agent/
  Pipeline/key creation, accepted ingest, duplicate retry, filtered query and
  Usage accounting against the live PostgreSQL-backed stack.
- Documented the port override and a standalone deployment path in README and
  updated STATUS evidence. The browser remains pending manual user acceptance;
  no remote push or deployment was performed.
- Fixed browser connection diagnostics after manual acceptance exposed a
  generic network error: Compose now permits both `localhost` and `127.0.0.1`
  Web origins for direct API access, the login form clearly recommends the
  same-origin `/api/v1` path, and network errors report the attempted API base
  without logging the credential.

## 2026-08-27 - Isolated implementation worktree

- Created checkpoint commit `6c5b1e6` after Go build, unit tests, race tests, vet,
  diff checks, and secret review passed.
- Created branch `codex/full-gline` in `E:\Proj\gline-full`.
- Selected a modular Go server, PostgreSQL, React/TypeScript web application,
  reliable Go Agent, and Docker Compose as the implementation baseline.

## 2026-08-27 - Full platform candidate

- Implemented domain models, PostgreSQL migrations and narrow repositories for
  Project, API Key, Agent, Pipeline, Batch, Entry, Usage, Retention, Quarantine
  and Audit. Migration execution uses an advisory lock and checksum drift
  detection.
- Implemented scoped HMAC API keys, bootstrap administration, transactional
  Control/Ingest/Operations services, bounded keyset Query, request IDs,
  structured HTTP errors, readiness, Prometheus HTTP metrics and maintenance.
- Added default retention to the Project creation transaction after runtime
  workflow review exposed a guaranteed 404 for newly-created Projects.
- Implemented reliable Agent file identity checkpoints, WAL recovery and
  compaction, retry dispatch, heartbeats, spool limits and Windows file rotation
  sharing. Corrected WAL compaction ordering so later rotation checkpoints are
  not rolled back during recovery.
- Implemented the React console and later removed all lint warnings by separating
  Provider files from Hook state, deriving Project selection during render and
  giving modal/search/retention state explicit component lifetimes.
- Removed the obsolete local `github.com/isyuah/testx => E:/Proj/testx` module
  replacement. Updated the reliable Agent example, README, ignores, Dockerfiles,
  Compose environment mapping and runbook.

## 2026-08-27 - Candidate verification and runtime blocker

- Passed full Go build, unit suite, race suite and vet.
- Passed frontend lint with zero warnings, four tests and production build.
- Passed Compose configuration rendering with temporary validation values and
  served the Vite application with an observed local HTTP 200.
- Added regression coverage for Project default retention, Quarantine replay
  failure release and per-Project query concurrency isolation.
- Real PostgreSQL integration and full Compose E2E remain unverified. Docker
  Desktop 4.49.0 repeatedly failed before engine startup while removing the
  stale `AppData\Local\Docker\run\dockerInference` socket reparse point. The
  Agent did not reset Docker, delete the socket or remove any volume.

## 2026-08-27 - Reliability review hardening

- Bound every successful Agent delivery to the exact outbound `batch_id` in the
  Server ACK. Missing, malformed, mismatched or unknown-success ACKs remain
  retryable and cannot advance the durable checkpoint.
- Split delivery failures into retryable, per-batch quarantine and systemic
  terminal classes. Per-batch 400/409/413/422 responses are durably quarantined
  in the WAL and no longer poison later batches; authentication and endpoint
  failures stop with the affected batch still pending.
- Added offline local-quarantine metadata listing and explicit discard commands.
  Quarantined payloads remain counted against spool capacity until discarded.
- Recovered rename/recreate rotation that happens while an Agent is stopped by
  persisting a new zero-offset file epoch before reading the replacement.
- Made API-key last-used timestamps a bounded best-effort observation instead
  of an authentication dependency, and added configurable query execution
  deadlines.
- Re-ran full unit, race and vet suites, native and Linux builds, frontend lint,
  tests and production build, Compose rendering and Vite HTTP smoke. All passed.
  The tagged PostgreSQL suite explicitly skipped its real database case because
  no test database URL is available; the full Compose workflow remains blocked
  by the Docker Desktop engine failure recorded above.
- Created a local checkpoint for the complete candidate. No remote push or
  deployment was performed.

## 2026-08-27 - CI and operational observability

- Added low-cardinality Server metrics for HTTP route templates, ingest/query
  outcomes, database-pool state and bounded maintenance steps. Business services
  depend only on narrow observer interfaces; Prometheus remains in composition.
- Added optional loopback-only Agent `/metrics` and `/livez` endpoints. Spool
  gauges derive from recovered WAL state, while delivery, parsing and Pipeline
  signals use bounded result labels and a configured Pipeline cap.
- Added `/livez` without database access and made `/readyz` reject immediately
  during draining. The Compose health check now uses readiness.
- Added GitHub Actions jobs for formatting/vet/unit/Linux builds, race tests,
  PostgreSQL 17 integration and frontend lint/test/build. Checkout permissions
  are read-only and no project secret is referenced.
- Added an application-level PostgreSQL integration workflow covering liveness,
  readiness, tenant resource creation, Agent-bound credentials, accepted and
  duplicate ingest, bounded query, usage accounting and metric cardinality. It
  creates and removes only its own random schema.
- Formatted previously unformatted tracked Go files so the new repository-wide
  formatting gate reflects a clean baseline; this was a mechanical change.
- Re-ran Go unit, tagged-build, race and vet checks, Linux builds, Web
  lint/test/build and Compose rendering; all available checks passed. The two
  real PostgreSQL tests skipped because no local database URL is available.

## 2026-08-27 - Ingest admission control

- Added a concurrency-safe, bounded in-memory token bucket with per-API-key
  request rate and per-Project entry, byte and in-flight budgets. Idle key and
  Project state is evicted; no high-cardinality identity appears in metrics.
- Placed admission after authentication and batch validation but before the
  database transaction. Request attempts consume the key rate; entry/byte cost
  is committed only for a new accepted batch and refunded for duplicate or
  failed transactions. In-flight reservations always release exactly once.
- Mapped temporary exhaustion to 429 plus `Retry-After`; the existing Agent
  transport classifies it as retryable and honors the delay without advancing
  its durable checkpoint. A batch larger than configured minute capacity gets
  a non-retryable 413 because it can never fit without configuration change.
- Documented that the MVP limiter is per Server instance. Horizontal replicas
  multiply effective capacity until a gateway or shared admission store is
  deliberately introduced with new consistency semantics.
- Aligned the existing per-Project Query semaphore with the same resource
  contract: capacity exhaustion now fails fast as 429 plus `Retry-After`, while
  request cancellation and database execution deadlines remain distinct. Query
  deadline failures now expose stable 504 `query_timeout` instead of a generic
  internal error.

## 2026-08-31 - Acceptance guide and handoff

- Added `docs/acceptance-and-usage.md` with the first-run Compose workflow,
  console resource creation order, Agent configuration, manual acceptance
  checklist, outage recovery, Pipeline control, troubleshooting and cleanup.
- Recorded the current Go code-size baseline: 122 files and 15,505 physical
  lines, including 87 production files / 10,255 lines and 35 test files /
  5,250 lines.
- Documented a staged reading strategy: first run the end-to-end flow, then
  read the architecture and core Agent/Server paths, and use focused module
  walkthroughs where needed.
