# Gline Development Protocol

## Scope

This worktree implements the complete Gline application described by
`docs/backend-tutorial/`. The backend tutorial is the authoritative target when
older documentation disagrees.

## Progress Records

- Update `STATUS.md` when a coherent slice becomes implemented, verified,
  integrated, runnable, or accepted.
- Append concise outcome and evidence entries to `DEVLOG.md` at slice completion,
  failed integration recovery, checkpoint commit, and handoff.
- Keep these evidence levels distinct: implemented, verified, integrated,
  runnable, accepted.

## Architecture Boundaries

- Start as a modular monolith. Do not add microservices, queues, or analytical
  stores without measured evidence and an ADR.
- Project identity comes from authentication context, never from an ingest or
  query request body.
- A successful ingest ACK is returned only after the PostgreSQL transaction
  commits.
- Idempotency is `(project_id, batch_id)` plus a server-computed canonical
  payload hash.
- Query endpoints require bounded time ranges, limits, and keyset pagination.
- HTTP adapters do not own SQL or domain rules. PostgreSQL adapters do not own
  authentication policy.

## Verification

- Run focused tests while implementing and the full Go/frontend suites before
  checkpoint commits.
- Integration claims require a real PostgreSQL-backed test or Compose run.
- UI build success is not visual acceptance. Record browser/runtime verification
  separately.
- Never commit secrets, local `.env` files, database volumes, or generated build
  output.
