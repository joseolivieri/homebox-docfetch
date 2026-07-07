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

Single Go binary, three subcommands, shared `internal/*` packages:

- `docfetch scheduler` — long-running; in-process cron drives scan/followup/reconcile jobs.
- `docfetch once` — run one scan pass and exit (for manual runs / testing / debugging).
- `docfetch portal` — Phase 2; HTTP server for photo intake. Reuses every core package.

### Doc-fetch pipeline (Phase 1)

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

### Phase 2 intake pipeline (additive)

```
phone -> portal (Tailscale-only PWA) -> capture 1-2 photos (model sticker and/or receipt)
  -> single multimodal LLM call: classify each photo + extract
       sticker -> {manufacturer, modelNumber, serialNumber, productType}
       receipt -> {purchaseFrom, purchaseDate, purchasePrice, (name hint)}
  -> confirm screen: user reviews/corrects fields; optional location pick (default unset)
  -> POST create entity (name [+ entityTypeId, tagIds=unverified+provenance, optional parentId])
  -> PATCH metadata (identity + purchase block)
  -> attach receipt image (type=receipt)
  -> SearXNG image search -> official product photo -> attach (type=photo, primary=true)
  -> existing discovery pipeline -> attach docs (type=manual)
  -> warranty: purchaseDate anchor; set hard warrantyExpires ONLY if confidently found,
     else write "est. <term> from <date>" into warrantyDetails, leave warrantyExpires null
```

## 4. External dependencies

| Dependency | Role | Notes |
|---|---|---|
| Homebox | Target inventory | Entity-model fork. Scoped API token. `homebox.jentaculum.net`. |
| SearXNG | Search backend | **Self-hosted** container (new), added by this feature. No API keys. Web + image search. |
| OpenRouter | LLM | 8B text rerank (Phase 1); cheap vision model (Phase 2). Key from vault. |
| ntfy | Notifications | Existing homelab service. Review-gate + reconcile digest. Topic `homelab`. |

## 5. Config schema (property file, YAML)

Canonical example. Secrets are env-interpolated (`${VAR}`), injected by Ansible from vault.

```yaml
homebox:
  url: https://homebox.jentaculum.net
  token: ${HOMEBOX_TOKEN}
  page_size: 100

schedule:
  scan_new:   "0 */6 * * *"        # discover + initial fetch on new items
  followup:   "0 4 * * 0"          # weekly re-check known items for new/updated docs
  followup_after: 720h             # only re-check items whose last fetch is older than this

discovery:
  searxng_url: http://searxng:8080
  queries:
    - "{manufacturer} {modelNumber} user manual filetype:pdf"
    - "{manufacturer} {modelNumber} datasheet pdf"
  max_candidates: 8
  min_pdf_bytes: 20000
  max_pdf_bytes: 52428800
  rate_limit_per_min: 20           # global outbound cap w/ jitter
  backoff_base: 24h                # not-found exponential backoff base

llm:
  base_url: https://openrouter.ai/api/v1
  api_key: ${OPENROUTER_API_KEY}
  rerank_model: "meta-llama/llama-3.1-8b-instruct"   # cheap 8B; tiebreak only
  vision_model: "google/gemini-2.0-flash-lite-001"   # Phase 2
  max_snippet_chars: 150

confidence:
  auto_attach_threshold: 0.7       # >= attach; below -> review-gate
  require_model_match: true

attach:
  doc_type: manual                 # native Homebox attachment enum
  skip_if_manual_exists: true

intake:                            # tags-based triage (labels are "tags" in this fork)
  unverified_tag: "docfetch/unverified"   # created at startup if missing
  provenance_tag: "source/docfetch"       # items created with no entityTypeId (no "Item" type exists)

reconcile:
  digest_schedule: "0 9 * * 1"     # weekly GET entities?tags=<unverified> -> ntfy count

notify:
  ntfy_url: http://ntfy:8080
  ntfy_topic: homelab

portal:                            # Phase 2 (dormant until then)
  listen: ":8099"
  location_entity_type: "Location" # source for optional location dropdown
  default_location: null           # unset by default
  intake_photos: [sticker, receipt]
  warranty_estimate: true

state_db: /data/docfetch.db
```

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
