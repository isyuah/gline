# Gline

Gline is a self-hosted, multi-tenant log management platform for personal
projects and small service clusters. A reliable Go Agent tails files into a
disk-backed WAL; a modular Go Server authenticates, validates and commits
idempotent batches to PostgreSQL; a React console manages projects, credentials,
agents, pipelines, search and operational policies.

The backend is deliberately a modular monolith. Control, ingest, query and
operations are separate logical planes with explicit domain and repository
boundaries, while one process keeps the initial deployment and transaction model
understandable. Horizontal scaling is a measured later-stage evolution, not a
collection of unused infrastructure.

## What Is Implemented

- Project isolation and scoped API keys whose plaintext is shown only once.
- Agent registration, heartbeat status and versioned Pipeline configuration.
- Strict versioned batch protocol, bounded requests and transactional
  idempotency on `(project_id, batch_id)` plus a canonical payload hash.
- PostgreSQL migrations, composite tenant foreign keys, usage buckets and audit
  events.
- Time-bounded log search with stable keyset cursors and per-project concurrency
  limiting.
- Default and configurable retention, bounded cleanup and durable Agent-local
  quarantine with explicit inspection/discard commands.
- Server-side Quarantine storage, lease recovery and replay/discard operations
  are ready for a future asynchronous processing stage; synchronous ingest does
  not acknowledge failed batches by parking them there.
- Health, database readiness, Prometheus metrics and graceful shutdown.
- Low-cardinality HTTP, ingest, query, database-pool and background-job
  metrics; optional loopback-only Agent spool/delivery metrics.
- File-identity checkpoints, copy-truncate and rename/recreate rotation handling,
  CRC-protected WAL recovery, bounded spool backpressure and retrying delivery.
- React administration console and a Docker Compose delivery topology.

## Architecture

```text
log files
   |
   v
Go Agent: durable file source -> parser -> disk WAL -> retry dispatcher
   |                                  batch + heartbeat
   v
Go Server
   +-- Control: Project / Key / Agent / Pipeline / Audit
   +-- Ingest: protocol / auth / validation / idempotent transaction
   +-- Query: project isolation / bounded filters / keyset cursor
   +-- Operations: retention / usage / quarantine / maintenance / metrics
   |
   v
PostgreSQL

React console -> same-origin /api proxy -> Go Server
```

The authenticated principal supplies the Project identity. Ingest and query
bodies cannot select another tenant. An ingest ACK is emitted only after the
PostgreSQL transaction commits; a retry after a lost response therefore returns
`duplicate` instead of duplicating entries.

## Run With Docker Compose

Prerequisites: Docker Desktop with Compose v2.

```powershell
Copy-Item .env.example .env
```

Replace all three `replace-with-...` values in `.env` with independent random
values, then start the existing Compose project:

```powershell
docker compose up --build -d
docker compose ps
```

Open `http://localhost:4173`. Sign in with the bootstrap token from `.env` and
an empty Base URL. The console talks to the Server through the web container's
same-origin proxy.

The intended first workflow is:

1. Create a Project. Gline creates its enabled 14-day retention policy in the
   same transaction.
2. Create an Agent, then create a Pipeline bound to that Agent.
3. Create an API key with `ingest` and `agent:write` scopes. Store the one-time
   secret securely.
4. Copy `examples/glineconf.yaml` to an ignored local file and replace the Agent
   ID, Pipeline ID and API key.
5. Create `data/demo-api.log`, start the Agent, then append lines to the file.
6. Use the Logs page to search the ingested entries; inspect Agents, Usage,
   Audit and Retention for the operational view.

```powershell
New-Item -ItemType Directory -Force data | Out-Null
New-Item -ItemType File -Force data\demo-api.log | Out-Null
Copy-Item examples\glineconf.yaml .glineconf
go run ./cmd/agent -config .glineconf
```

In another terminal, append a line:

```powershell
Add-Content -LiteralPath data\demo-api.log -Value '{"level":"info","message":"hello from gline"}'
```

Useful endpoints are `http://localhost:8080/livez`, `/readyz` and `/metrics`.
`/livez` never checks PostgreSQL; `/readyz` checks the database and turns
unavailable as soon as graceful shutdown begins.
Stop the stack without deleting its named PostgreSQL volume:

```powershell
docker compose down
```

An immutable batch rejected with a per-batch 400/409/413/422 response is moved
to the Agent WAL's local quarantine so later batches can continue. Stop the
Agent before managing that WAL. Listing reports metadata only, not log payloads:

```powershell
go run ./cmd/agent -config .glineconf -quarantine-list
go run ./cmd/agent -config .glineconf -quarantine-discard <batch-id>
```

Authentication, authorization and endpoint errors stop delivery with the batch
still pending because those failures normally require fixing shared Agent
configuration rather than discarding data.

The reliable example explicitly enables Agent operations at
`http://127.0.0.1:9109/metrics` and `/livez`. `metrics_addr` only accepts a
loopback host; omit it to disable this listener. Its gauges are calculated from
the recovered WAL state, so a process restart does not reset the visible
backlog.

The Server-side quarantine table and console tab are deliberately separate from
this Agent-local path. They become active when a later asynchronous parser or
indexer can durably persist a failed payload before acknowledging it. The
current synchronous ingest path returns an error and leaves the Agent copy
authoritative instead of weakening commit-after-ACK semantics.

## Local Development

Run PostgreSQL first, then set the required Server variables in the current
PowerShell session. Tokens and peppers must contain at least 24 characters.

```powershell
$env:GLINE_DATABASE_URL = 'postgres://gline:password@127.0.0.1:5432/gline?sslmode=disable'
$env:GLINE_BOOTSTRAP_TOKEN = 'replace-with-local-bootstrap-token'
$env:GLINE_API_KEY_PEPPER = 'replace-with-local-api-key-pepper'
go run ./cmd/server
```

The Server applies embedded checksum-verified migrations at startup. For the web
console, use a second terminal:

```powershell
Set-Location web
pnpm install
pnpm dev
```

Vite proxies `/api`, `/healthz`, `/livez` and `/readyz` to `localhost:8080`; open
`http://localhost:5173` and leave Base URL empty.

## Verification

```powershell
go build ./cmd/...
go test ./... -count=1
go test -race ./... -count=1
go vet ./...

Set-Location web
pnpm lint
pnpm test
pnpm build
```

PostgreSQL integration tests are opt-in because they create temporary schemas
and exercise both repositories and the full HTTP-to-database application
workflow:

```powershell
$env:GLINE_TEST_DATABASE_URL = 'postgres://gline:password@127.0.0.1:5432/gline_test?sslmode=disable'
go test -tags=integration ./... -count=1 -v
```

Do not point this test at a database containing useful data.

## Documentation

The implementation-oriented backend course starts at
[`docs/backend-tutorial/README.md`](docs/backend-tutorial/README.md). It explains
the architecture, state machines, transaction boundaries, failure semantics,
testing strategy and the later evidence-driven route to horizontal scaling and
high availability. Older documents remain background material; the backend
tutorial is the authoritative reading path when they disagree.

## Security Boundary

- Never commit `.env`, `.glineconf`, WAL files, logs or real API keys.
- Bootstrap credentials administer every Project and must stay on a trusted
  control surface; Agents should receive only project-scoped keys.
- TLS termination is expected in front of the Compose topology for any network
  beyond localhost.
- The current platform targets a trusted self-hosted environment. Before public
  multi-tenant exposure, add external identity, request-rate enforcement,
  encrypted secret management and deployment-specific network controls.
