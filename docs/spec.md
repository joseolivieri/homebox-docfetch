# homebox-docfetch — Specification

Status: design locked for Phase 1 pending 3 open questions (see [decisions.md](decisions.md)).
Audience: implementing agents. This is the stable "what/why". Task execution lives in the phase boards.

---

## 1. Goals

- Regularly scan a Homebox collection and **auto-fetch user manuals / support docs** for items.
- Attach found docs to the item as native Homebox attachments (`type=manual`), stored locally by Homebox.
- **Property-file managed schedules**: distinct cadence for (a) discovering + initial-fetch on new items,
  (b) follow-up re-checks for updated/new docs on known items.
- Efficient for a homelab: tiny footprint, cheap external calls, no wasted work, no junk attachments.
- **Phase 2**: phone-friendly portal — photo a model label and/or receipt → identify item → auto-create in
  Homebox → infer metadata → fetch official product photo → run the same doc-fetch pipeline.

## 2. Non-goals

- No Homebox UI integration. All interaction is via the REST API with a scoped API token.
- No direct Homebox DB access.
- No deep-reading of manual PDFs by the LLM (budget guard — see §7).
- Phase 1: no item creation from photos, no vision. (Client still gains create/patch for Phase 2 reuse.)

## 3. Architecture

**Two discrete stages** (D20). Stage names are used across config, docs, and notifications:

| Stage | Process | Remote calls allowed |
|---|---|---|
| **Intake** | `docfetch portal` (package `internal/portal`) | Homebox API + **vision model ONLY** — no web searching, so the LLM can move local/offline later |
| **Curation** | `docfetch scheduler` (package `internal/scheduler`) | Everything: SearXNG searches, doc downloads, image fetches, LLM — enrich, docs, official photos, warranty, tagging |

The stages share no database (separate SQLite volumes); **Homebox itself is the
bus** between them — intake creates/annotates entities, the scanner's
change-poll notices (~30s) and curates. The ntfy Attach/Reject buttons obey the
same boundary: the portal endpoints only write queued `approved`/`rejected`
notes lines; the scanner downloads/labels on its next pass.

Single Go binary, three subcommands, shared `internal/*` packages:

- `docfetch scheduler` — curation stage; in-process cron drives scan/followup/reconcile jobs.
- `docfetch once` — run one curation pass and exit (for manual runs / testing / debugging).
- `docfetch portal` — intake stage; HTTP server for photo intake.

### Curation: doc-fetch pipeline

```
scheduler tick
  scan job:   GET entities (paginated) -> diff vs SQLite (new? metadata changed? updatedAt newer?)
                new item        -> discovery(initial)
                stale/changed   -> discovery(followup) if followup interval elapsed
  discovery(item):
    build queries from {manufacturer, modelNumber, name}
      -> SearXNG search            -> candidate URLs (title, url, snippet)
      -> rules filter/score        -> model# match, application/pdf, size sanity
      -> if ambiguous: LLM rerank  -> OpenRouter 8B picks best + confidence (tiebreak only)
      -> dedupe by content SHA-256 -> vs entity's existing attachments
      -> confidence gate:
           high  -> download -> POST attachment type=manual
           low   -> ntfy review notification, mark pending   (default gate; see decisions.md)
    record result in SQLite (status/hash/url/attempts/last_checked)
  reconcile job: GET entities?tags=<unverified> -> weekly ntfy "N items awaiting review" digest
```

### Intake pipeline (portal — vision-only)

```
phone -> portal (Tailscale-only PWA) -> capture up to 4 photos (sticker/receipt/product/warranty)
  -> single multimodal LLM call: classify each photo + extract
       sticker  -> {manufacturer, modelNumber, serialNumber, productType}
       receipt  -> {purchaseFrom, purchaseDate, purchasePrice, (name hint)}
       warranty -> {months, claims URL}
  -> confirm screen: user reviews/corrects fields; quantity; optional location pick
  -> POST create entity (tagIds=unverified+provenance, optional parentId)
  -> PUT metadata (identity + purchase + photo-read warranty)
  -> local QR decode over the photos (pure Go, no network): http(s) support links
     from manufacturer-printed codes -> user-confirmable on the confirm screen
     -> stored as "- qr [link](url)" notes lines + "Support (QR)" custom field
  -> attach the intake photos (receipt/warranty/product-personal[primary]/sticker)
  -> DONE. No web calls. The curation stage takes over via change-poll (~30s).
```

### Curation extras (scanner — moved from the portal, D20)

```
per processed item, after the doc phase:
  photo:    no "product-official" attachment yet -> SearXNG image search
            -> vision ranks candidates against the user's personal photo
               (downloaded back from Homebox as the reference)
            -> conf >= curation.photo.min_confidence -> attach + notes line
  warranty: purchaseDate set, no expiry, not lifetime -> web search + LLM read
            -> set hard warrantyExpires ONLY if confidently found + sourced,
               else "est. <term> (unverified)" into warrantyDetails (D11)
  both record photo/warranty rows in the decisions ledger (retry-throttled)
```

## 4. External dependencies

| Dependency | Role | Notes |
|---|---|---|
| Homebox | Target inventory | Entity-model fork. Scoped API token. `homebox.jentaculum.net`. |
| SearXNG | Search backend | **Self-hosted** container (new), added by this feature. No API keys. Web + image search. |
| OpenRouter | LLM | 8B text rerank (Phase 1); cheap vision model (Phase 2). Key from vault. |
| ntfy | Notifications | Existing homelab service. Review-gate + reconcile digest. Topic `homelab`. |

## 5. Config schema (property file, YAML)

Canonical, fully-commented example: [`config.example.yaml`](../config.example.yaml).
Secrets are env-interpolated (`${VAR}`), injected by Ansible from vault.

The schema mirrors the two stages (D20). Skeleton:

```yaml
# shared
homebox:  {url, token, page_size}
llm:      {base_url, api_key, rerank_model, vision_model, max_snippet_chars}
tags:     {unverified, provenance}        # triage/provenance (labels are "tags" in this fork)
notify:   {ntfy_url, ntfy_topic, ntfy_token}
notes:    {audit_log}                     # opt-in terse line per derived write, with confidence;
                                          # URLs always render as [pdf](…)/[web](…)/[src](…)
state_db: /data/docfetch.db

# stage 1: item intake (portal) — vision-model calls only, no web searching
intake:
  {listen, public_url, location_entity_type, photos}

# stage 2: curation (scanner) — ALL web egress
curation:
  schedule:  {scan_new, followup, followup_after, change_poll}
  discovery: {searxng_url, language, pipeline, queries, max_candidates,
              min/max_pdf_bytes, rate_limit_per_min, backoff_base}
  docs:      {enabled, skip_if_exists, auto_attach_threshold, require_model_match,
              classes: [{name, field, attach_as, keywords, queries, categories, enabled}]}
             # each class selects + attaches its own best doc; manual is primary
             # (drives status + review-gate); categories gate a class to matching
             # item tags/name (parts -> appliances/tools). See D23.
  enrich:    {enabled, fill_only, auto_write_threshold, min_agreeing_sources, back_check, fields}
  photo:     {enabled, min_confidence}    # official product photo
  warranty:  {enabled}
  reconcile: {digest_schedule}
```

### Learning feedback loop (Phase A)

Every doc-pipeline verdict lands in the scheduler's SQLite `decisions` table
(entity, `doc_class`, stage, chosen URL, confidence, compact candidate-set
JSON, outcome `attached|linked|review|notfound`). Labels arrive asynchronously:

- **rejected** — ntfy Reject button (portal `/api/reject`, verb-scoped HMAC)
  writes `rejected [link](url)` into the entity's docfetch notes block; the
  scheduler ingests it on the next scan and permanently filters the URL from
  candidates. Hand-written `rejected` lines label the same way (src `manual`).
- **overridden / rejected(src=override)** — weekly reconcile diffs Homebox
  against what was written: a removed manual → rejected + re-search; a changed
  machine-filled metadata value → enrichment row `superseded` (never refilled).
- **confirmed (src=age)** — an attachment surviving 30 days untouched.

The weekly digest includes a 7-day outcome/label snapshot. The ledger is the
dataset for the next phases: eval golden set, learned domain priors, and
threshold calibration.

## 6. Homebox API reference (AUTHORITATIVE — verified against live swagger 2026-07-06)

**This fork uses a unified `entity` model. Classic Homebox `/items`, `/labels`, `locationId` do NOT exist.**
Base path `/api/v1`. Bearer token auth.

### Entities (items are entities of a given entity-type)
- `GET  /v1/entities` — list. Query: `q`, `page`, `pageSize`, `tags`, `parentIds`.
  Wrapper: `{page, pageSize, total, items:[...], totalPrice}` — results under `.items`. `total:0` is valid, not an error.
  Each item is an `EntitySummary`: `id, name, description, entityType, tags[], parent, assetId, imageId,
  thumbnailId, quantity, insured, archived, itemCount, purchasePrice, createdAt, updatedAt`.
  → Use `updatedAt` for change detection; `imageId`/`thumbnailId` to know if a product photo exists.
- `POST /v1/entities` — create. Body `EntityCreate`, **required: `name` only.**
  Optional: `entityTypeId`, `tagIds[]`, `parentId`, `description`, `quantity`. **No location field.**
- `GET  /v1/entities/{id}` — detail `EntityOut`: adds `manufacturer, modelNumber, serialNumber,
  notes, fields[], attachments[], children[], purchaseDate/From/Price, warrantyExpires/Details,
  lifetimeWarranty, syncChildEntityLocations, ...`. → Read `attachments[]` before attaching (dedupe).
- `PATCH /v1/entities/{id}` — partial update. **Verified gotcha:** PATCH honors `tagIds`/`archived`
  and preserves other fields, but **silently ignores scalar metadata** (manufacturer, modelNumber,
  serialNumber, purchase*, warranty*). Use PATCH for tag changes only. Use **`PUT` for metadata writes**
  (full replace — fetch + merge first to avoid blanking). Phase 1 only PATCHes tags; Phase 2 uses PUT.
  `EntityUpdate` fields: `manufacturer, modelNumber, serialNumber, assetId, notes, fields, tagIds,
  purchaseDate, purchaseFrom, purchasePrice, warrantyExpires, warrantyDetails, lifetimeWarranty,
  archived, insured, quantity, parentId, syncChildEntityLocations, ...` (required: `name`).
- `DELETE /v1/entities/{id}`.

### Location = a parent entity
There is no `locationId`. "Location" is an entity (its own entity-type); an item's location is its
`parentId`. Phase 1 creates items parent-less (triage is by tag). Phase 2 portal optionally sets `parentId`.

### Entity types
- `GET /v1/entity-types`, `GET/PUT/DELETE /v1/entity-types/{id}`, create via `EntityTypeCreate`.
- **Live reality (verified 2026-07-06):** only a `Location` type exists (`isLocation:true`). **There is no "Item" type.**
  Items are created with **no `entityTypeId`**. Do NOT try to resolve an "Item" type at startup.
  Phase-2 location dropdown = entities whose entity-type has `isLocation:true`.

### Tags (this fork's "labels")
- `GET /v1/tags` (list), `POST /v1/tags` (`TagCreate`, required `name`; has `color, icon, parentId`),
  `GET/PUT/DELETE /v1/tags/{id}`.
  → Ensure `unverified_tag` / `provenance_tag` exist at startup (create if missing); resolve to ids.
  → Reconcile digest = `GET /v1/entities?tags=<unverified_id>`.

### Attachments (stored locally by Homebox)
- `POST /v1/entities/{id}/attachments` — multipart formData: `file` (req), `name` (req),
  `type` (optional), `primary` (bool). **`type` enum:** `photo, manual, warranty, attachment,
  receipt, thumbnail`. Docs → `type=manual`. Product photo → `type=photo, primary=true`. Receipt → `type=receipt`.
- `PUT/DELETE /v1/entities/{id}/attachments/{attachment_id}` — update (`title, type, primary`) / delete.
- `POST /v1/entities/{id}/attachments/external` — references an external storage source
  (`external_id, source_type, attachment_type, title`). **NOT a URL-fetch.** We do standard multipart
  upload so we control content-hash dedupe.

## 7. Efficiency & cost guardrails

- **Diff, don't sweep.** Only new / `updatedAt`-changed / metadata-changed entities enter the pipeline.
- **Negative-result cache + exponential backoff** (`backoff_base`) on not-found — don't re-search every tick.
- **Rules-first, LLM last.** If a candidate exact-matches model# and is `application/pdf`, attach with no
  LLM call. LLM rerank fires only in the ambiguous zone. Most items never call the LLM.
- **Never deep-read PDFs with the LLM.** It sees only `{title, url, snippet(<=150 chars)}`.
  Constrained JSON output `{"best":<int|-1>,"confidence":0-1}` (~15 out tokens).
- **Content-hash dedupe** → follow-ups never re-attach identical docs; only newer versions attach.
- **`skip_if_manual_exists`** → never fights a manually-added manual.
- **Global egress rate-limit + jitter** on search/download.
- Budget target: rerank cost ~pennies/mo at homelab volume; Phase 2 vision is user-initiated, low volume.
