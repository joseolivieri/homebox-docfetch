# Decisions & open questions

Format: decisions are immutable once logged (append a new entry to reverse one). Open questions
**block** the tasks that reference them — do not guess; resolve with the user first.

## Locked decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | **Go** as the implementation language. | Static binary, tiny idle footprint, in-process cron, matches Homebox's stack, first-class multipart/SQLite. |
| D2 | Single binary, subcommands `scheduler`/`once`/`portal`; core logic in `internal/*` packages. | Phase 2 portal reuses all core packages; no rewrite. |
| D3 | **SearXNG (self-hosted)** is the search backend. | Homelab-native, no API keys, aggregates multiple engines, adds one small container. |
| D4 | **OpenRouter 8B text model** reranks candidates — **tiebreaker only**, after rules. | Ranking is short-text classification; 8B is ample. Rules-first keeps most items LLM-free. Budget-safe. |
| D5 | Never deep-read PDFs with the LLM; it sees only title/url/snippet. Constrained JSON out. | Protects the ~$5/mo OpenRouter budget. |
| D6 | Attach docs as native `type=manual`; product photo `type=photo primary=true`; receipt `type=receipt`. | Uses Homebox's first-class attachment enum; Homebox stores locally. |
| D7 | Standard multipart upload (download → hash → upload), **not** `/attachments/external`. | `external` endpoint references external storage, not URL-fetch; we need content-hash dedupe. |
| D8 | Triage/status via **tags** (`unverified`, `provenance`), not location. | This fork renamed labels→tags; tags survive a location/parent change and are filterable (`?tags=`). |
| D9 | **Location dropped from create.** Item location = parent entity (`parentId`), optional, Phase-2 only, default unset. | `EntityCreate` requires only `name`; no `locationId` exists. Tags handle triage; rooms are a later, optional parent assignment. |
| D10 | Phase 2 intake supports **model sticker and/or receipt**, single multimodal call. Receipt fills purchase block + attaches as `type=receipt`. | Maps 1:1 to native fields; one call is cheaper and shares context. |
| D11 | Warranty: set hard `warrantyExpires` only when confidently found; else estimate into `warrantyDetails`, leave `warrantyExpires` null. | Keep date fields trustworthy; no fabricated expiries from guesses. |
| D12 | Deploy as a new `ansible/roles/docfetch/` role following the `homebox` role pattern; secrets in vault; update `docs/architecture.md` in the landing commit. | Homelab conventions (repo CLAUDE.md). |
| D13 | Metadata enrichment (Phase 1.5) is **fill-only**: machine writes only empty fields, never overwrites human-entered values. | Eliminates the worst failure mode (corrupting curated data) outright. |
| D14 | Enrichment confidence = **corroboration**: ≥2 independent domains agree + back-check round-trip passes. LLM self-scores alone never authorize a write. | LLM confidence is uncalibrated; agreement + round-trip search is verifiable evidence. |
| D15 | Per-field gating with provenance: each field gates independently; every machine write appends a `docfetch:` note + keeps `docfetch/unverified` until human-blessed; full audit in SQLite `enrichments` table (undo-able). | Trust and reversibility; machine data always distinguishable from human data. |
| D16 | Enrich runs **before** doc-fetch in the same scan pass; all metadata writes via `PutEntity` (fetch→merge→PUT). | Filled model# upgrades the manual query in the same cycle; PATCH silently drops scalar metadata. |

## Resolved (were blocking)

| # | Question | Resolution |
|---|---|---|
| Q1 | Homebox API token | Provided. Stored in vault as `docfetch_homebox_token`. Auth = `Authorization: Bearer <token>`. Verified live 2026-07-06. |
| Q2 | OpenRouter key isolation | **Separate key** (no prior OpenRouter key existed in vault). Stored as `docfetch_openrouter_api_key`. |
| Q3 | Scheduler confidence gate | **ntfy review-gate** confirmed — low-confidence notifies, does not attach. |

## Live API findings (verified against the instance 2026-07-06 — supersede earlier assumptions)

- **No "Item" entity-type exists.** Only one entity-type: `Location` (`isLocation:true`, id `e27d5012-5190-406e-80e0-36a3d0429de4`).
  → Items are created with **no `entityTypeId`**. The startup step does NOT resolve an "Item" type (P1-03 simplified).
  → For the Phase-2 location dropdown, list entities of the `Location` type (`isLocation:true`).
- **`GET /v1/entities` wrapper:** `{page,pageSize,total,items:[...],totalPrice}` — items under `.items`, not a bare array.
- **Collection is currently empty** (`total:0`; instance created 2026-06-27). Scheduler no-ops until items exist; Phase-2 portal will populate it. Do not treat "0 results" as an error.
- **Tags already seeded** (Appliances, Electronics, General, …). Our `docfetch/unverified` + `source/docfetch` tags do not exist yet → bootstrap creates them.
- **Secret → env mapping** (Ansible role injects vault → container env): `docfetch_homebox_token → HOMEBOX_TOKEN`, `docfetch_openrouter_api_key → OPENROUTER_API_KEY`.

## Deferred / future (not in scope now)

- **Doc classes (manual + quickstart + datasheet).** Today the pipeline attaches exactly ONE doc
  per item (single Best candidate; `skip_if_manual_exists` is global, so a quick-start guide never
  attaches once a manual exists). Deferred design (agreed 2026-07-06): classify candidates into
  configurable classes (`manual`/`quickstart`/`datasheet`, per-class `max`+`enabled`), rerank call
  becomes classify+rank in one call (`{"picks":[{class,best,conf}]}`, ~30 extra out-tokens), attach
  loop best-of-class (all Homebox `type=manual`, distinguished by attachment title suffix), per-class
  skip + content-hash dedupe via a small `docs` state table (same pattern as `enrichments`),
  confidence gate per class. Default: manual+quickstart on, datasheet off, max 1 each. ~1-2h work.
- Multi-item receipts (one receipt → parse line items → create N entities).
- Multi-user / multi-household (each = a Homebox group = a separate scoped token).
- Evaluating `/attachments/external` if Homebox later gains true URL-fetch semantics.
