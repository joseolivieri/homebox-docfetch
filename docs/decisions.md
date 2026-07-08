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
| D17 | **Learning ledger (Phase A):** every pipeline verdict (stage, candidate set, scores, outcome) is a `decisions` row; labels (`confirmed`/`rejected`/`overridden`) arrive later from the ntfy Reject button, override detection in reconcile, and 30-day survival. `doc_class` column reserved for future doc types. | Turns real usage into a labeled dataset for domain priors, threshold calibration, and a regression golden set — no model fine-tuning needed on an 8B budget. |
| D18 | **Rejections travel through Homebox notes**, not shared SQLite: the Reject button writes `rejected [link](url)` into the docfetch notes block; the scheduler ingests it on the next scan (change-poll makes that ~30s) and never proposes that URL again. Hand-written `rejected` lines work too. Action links are verb-scoped HMAC (`approve`/`reject` cannot be cross-replayed). | Portal and scheduler keep separate SQLite volumes (no cross-process locking); the entity itself is the shared bus, and the label is user-visible/editable. |
| D19 | **All URLs in notes and custom fields render as short markdown links** (`[pdf](…)`, `[web](…)`, `[src](…)`); `notes.audit_log` opt-in adds terse lines with confidence for every derived write (intake photos, official photo, warranty, metadata). Superseded enrichments (user changed/cleared a machine value) are never machine-refilled. | Raw URLs are noise in the UI; the audit trail makes every machine write inspectable; a human correction is final. |
| D20 | **Two discrete stages with a hard egress boundary.** Stage 1 **intake** (portal): vision-model calls ONLY — photo extraction + entity creation; no web searching, no downloads. Stage 2 **curation** (scanner): ALL web egress — enrichment, docs, official photos, warranty, tagging. Official-photo fetch + warranty estimate moved portal→scanner; the ntfy Attach button now queues an `approved [pdf](url)` notes line (like Reject) that the scanner fulfils (~30s, no content verify — human approved). Config/docs organized by stage (`intake:` / `curation:` sections). | Clean seam for moving the LLM local/offline (intake needs only vision); single writer for every web-derived artifact; the two processes already share nothing but Homebox — the notes block is the bus. |
| D21 | **Label QR codes are a first-class doc source — decoded locally at intake, followed by the scanner.** QR decoding is pure-local (gozxing; no network/LLM), so the intake boundary holds. Decoded http(s) links (app-store/social/payment codes filtered, user-confirmable) are stored as `- qr [link](url)` notes lines + a `Support (QR)` field. The scanner's `qr` pipeline stage (default FIRST: `[qr, brand-site, web-pdf, web-html]`) resolves redirects, harvests the target page's PDFs as Official+ModelMatch candidates (the physical label IS the product), and seeds the brand-domain cache from the final host. | Manufacturer-printed provenance beats any search result; when the QR page carries the manual, zero searches and zero rerank/brand-domain LLM calls happen. Treated as a definitive *starting point*, not a definitive document — targets are usually landing pages/shortlinks. |

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

- **Provider standardization (agreed 2026-07-08 — do as the FIRST step of the next new
  curation source).** Formalize the four curation steps (docs/enrich/photo/warranty) behind a
  common interface, enabling drop-in sources (RecallProvider, SpecificationProvider,
  EnergyGuideProvider…). NOT the flat `Enrich(ctx, item) (*Result, error)` shape — there is no
  shared result type, and the write side is where all safety lives (fill-only, corroboration,
  full-merge PUT, single-writer, ledger). Agreed shape — **providers propose, the worker
  disposes**:

  ```go
  type Provider interface {
      Name() string                                        // ledger doc_class
      Provide(ctx context.Context, it Item) ([]Proposal, error)
  }
  // Proposal: {kind: field|attachment|tag|note, target, value/bytes,
  //            confidence, source URL, official bool}
  ```

  Worker loop: run enabled providers → apply gates (thresholds, fill-only, dedupe, rejected-URL
  filter) → ONE merged PUT + uploads → one ledger row per proposal → notes lines. Learning loop
  becomes uniform (every proposal labelable; domain priors per provider). Port the existing four
  onto the interface in the same change — mechanical once the worker exists. Per-provider
  `enabled` config flags already exist (docs/enrich/photo/warranty).
- **Classic-Homebox API adapter (general-use blocker).** The `internal/homebox` client speaks
  only this deployment's entity-model fork (`/entities`, tags, parentId). Upstream Homebox uses
  `/items`, `/labels`, `locationId`. General-use release needs an API-flavor adapter interface
  with two implementations, plus feature-detection or a config switch. Decide fork-only vs
  upstream support before investing.
- **Learning phases B–D** (ledger from D17 is the dataset): B = golden set + `docfetch eval`
  replay subcommand (regression-test prompt/pipeline/threshold changes against confirmed
  item→doc pairs); C = learned domain priors (Laplace-smoothed accept/reject rate per domain
  from labels, seeds today's hard blocklists) + persisted brand-domain / per-manufacturer
  model-number-pattern memory; D = auto-calibrated confidence thresholds (observed precision
  per confidence band, target ~95%) + richer digest metrics.
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
