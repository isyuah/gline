# Gline Development Log

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
