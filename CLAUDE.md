# homebox-docfetch — Claude Instructions

Go sidecar that enriches a Homebox inventory (the **entity model** — upstream since the
June 2026 release; NOT the pre-entity API) purely via its REST API. Two strictly
separated stages, one process (`docfetch serve`, D25):

- **Intake** (`internal/portal`) — phone photo-intake PWA. Vision-model
  calls only; NO web egress (offline-LLM-ready). The boundary is package-level:
  `internal/portal` must never import discovery/egress code.
- **Curation** (`internal/scheduler`) — recurring scanner owning ALL
  web egress: metadata enrichment (fill-only, corroborated), per-class document fetching
  (manual/parts/quickstart/datasheet), official photos, warranty, tagging.

The entity **notes block** is currently the bus between them
(`- qr/rejected/approved [label](url)` semantic lines, `internal/notes`); it is being
replaced by a shared sqlite event store — see `docs/plan-architecture-v2.md` (M2/D26).

## Read order (before touching code)

1. `docs/spec.md` — goals, config schema, **authoritative Homebox API reference**. Do NOT
   re-derive API facts from classic-Homebox knowledge; this fork uses `/api/v1/entities`,
   tags (not labels), `parentId` locations.
2. `docs/decisions.md` — locked decisions D1–D24 + backlog. **Append a new D-row whenever a
   design decision is made**; that file is the design memory.
3. `docs/how-it-works.md` — plain-language pipeline walkthrough.
4. Phase boards: `docs/phase-1-scheduler.md`, `docs/phase-1.5-enrich.md`, `docs/phase-2-portal.md`.

## Hard-won API/runtime facts (violations caused real bugs)

- **PUT is full-replace**: round-trip `Fields` and `Parent` or they get wiped. PATCH ignores
  scalar changes. See `fullUpdateFrom` in the scheduler.
- **Server caps entity notes length (~1000)**: oversized notes → PUT 500 that loses EVERYTHING
  in the payload. `internal/notes` budgets/prunes; keep it that way.
- **Attachment deletes do NOT bump entity `updatedAt`** — only the per-item sweep sees removals;
  the 30s change-poll cannot.
- **Own writes bump `updatedAt`** → any tag/notes write can self-trigger the change-poll.
  Review-gate dedupe + idempotent tagging exist for this; preserve the pattern.
- Attachment `type` enum: manual/photo/receipt/warranty/attachment. Auth: `Bearer hb_k-…`.

## Conventions

- Single static binary (CGO-free, modernc sqlite), distroless image, in-process cron.
- Everything schedule/threshold/tag-related is config (`config.example.yaml`), never hardcoded.
- Secrets from env only (`HOMEBOX_TOKEN`, `OPENROUTER_API_KEY`, `NTFY_TOKEN`); never in the
  config file, never committed.
- Idempotent + cheap: diff don't re-sweep, cache negatives, content-hash dedupe, rate-limit egress.

## Build / test / release

```bash
go build ./... && go test ./...   # keep green; ~31 tests
```

CI (`.github/workflows/docker.yml`) publishes `ghcr.io/joseolivieri/homebox-docfetch:latest`
on every push to main (semver on `v*` tags). Pushing to main IS the release. The repo's
docker-compose.yml is for local/dev builds; production deployment lives in a separate
(private) infra repo that pulls the GHCR image.

If `CLAUDE.local.md` exists (gitignored), it holds the author's deployment-specific context —
read it too.
