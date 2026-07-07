# Phase 2 — Photo-intake portal

Deliverable: a phone-friendly, Tailscale-only web portal that turns a model-label and/or receipt photo into a
fully-created, doc-enriched Homebox entity — no Homebox login required by the user.

**Do not start until Phase 1 has landed.** Phase 2 is purely additive: it adds `internal/portal`, a vision code
path in `internal/llm`, and reuses `homebox`/`discovery`/`store`/`config`/`notify` unchanged.

Read [spec.md](spec.md) §3 (intake pipeline) and §6 (API — create/patch/tags/attachments) first.

Task IDs stable. Each: intent · files · acceptance · deps. Status `[ ]/[~]/[x]`.

---

- [x] **P2-00 · Vision LLM path**
  - Intent: extend `internal/llm` with a multimodal call (config `vision_model`). Single call accepts 1–2 images,
    classifies each (sticker vs receipt), returns structured JSON:
    `{sticker:{manufacturer,modelNumber,serialNumber,productType}, receipt:{purchaseFrom,purchaseDate,purchasePrice,nameHint}}`
    with per-field confidence. Missing photo → that block null.
  - Files: `internal/llm/`.
  - Acceptance: real sticker photo → correct identity fields; real receipt → purchase fields; both-in-one-call works;
    output is strict JSON; token use bounded.
  - Deps: Phase 1 `internal/llm`, Q2.

- [x] **P2-01 · Portal HTTP server + capture UI**
  - Intent: `docfetch portal` subcommand. Minimal PWA: camera capture for up to 2 photos (sticker/receipt),
    submit → intake handler. Served on `portal.listen`, Tailscale-only.
  - Files: `internal/portal/`, `cmd/docfetch/`, embedded static assets.
  - Acceptance: loads on phone over Tailscale; camera capture works; POST reaches the handler with images.
  - Deps: P2-00.

- [x] **P2-02 · Location dropdown (optional)**
  - Intent: on load, `GET /v1/entities` filtered to `portal.location_entity_type` → dropdown of locations.
    Default selection unset (`default_location`). No create-location from portal.
  - Files: `internal/portal/`, `internal/homebox/` (list-by-type helper).
  - Acceptance: dropdown lists existing locations; unset is valid and default; chosen value becomes `parentId` on create.
  - Deps: P2-01.

- [x] **P2-03 · Confirm screen**
  - Intent: after vision extraction, render editable fields (identity + purchase + name + optional location) for
    user correction before commit. Handles OCR errors on prices/dates.
  - Files: `internal/portal/`.
  - Acceptance: pre-filled from extraction; edits persist to the create/patch payload; user can cancel.
  - Deps: P2-02.

- [x] **P2-04 · Create + enrich orchestration**
  - Intent: on confirm — `POST /v1/entities` (name [+ tagIds=unverified+provenance, optional parentId])
    → **`PutEntity` (PUT)** identity + purchase block (PATCH silently ignores scalar metadata — see spec §6)
    → attach receipt (`type=receipt`) if present.
  - Files: `internal/portal/`, reuse `internal/homebox`.
  - Acceptance: entity appears in Homebox with correct type, tags, metadata; receipt attached; parentId set when chosen.
  - Deps: P2-03, Phase-1 P1-02/P1-03.

- [x] **P2-05 · Product photo fetch**
  - Intent: SearXNG image search from `{manufacturer, modelNumber}` → pick best (rules + optional LLM) → download →
    attach `type=photo, primary=true`. Skip if the entity already has an image.
  - Files: `internal/discovery/` (image search), `internal/portal/`.
  - Acceptance: representative product image attached as primary; low-confidence → skip (no junk photo), logged.
  - Deps: P2-04, Phase-1 discovery.

- [x] **P2-06 · Doc fetch on intake**
  - Intent: run the Phase-1 discovery pipeline synchronously (or enqueue) for the new entity → attach manuals.
  - Files: `internal/portal/`, reuse `internal/discovery`.
  - Acceptance: manuals attached (or review-gated) for the freshly-created item, same rules as scheduler.
  - Deps: P2-04, Phase-1 P1-05/P1-06.

- [x] **P2-07 · Warranty inference**
  - Intent: purchaseDate anchor + manufacturer-term lookup (SearXNG `"<mfr> <model> warranty"` + LLM extract, or LLM
    knowledge). Set hard `warrantyExpires` only if confident; else write "est. <term> from <date>" to `warrantyDetails`.
  - Files: `internal/portal/`, `internal/discovery/`.
  - Acceptance: confident case sets a real expiry; uncertain case leaves `warrantyExpires` null with an estimate note.
  - Deps: P2-04.

- [x] **P2-08 · Portal deploy**
  - Intent: expose the portal via the docfetch role — add the `portal` subcommand/container, Tailscale-only hostname
    (`*.ingress-1.jentaculum.net`), update compose + role.
  - Files: `ansible/roles/docfetch/`, `docker-compose.yml`, `docs/architecture.md`.
  - Acceptance: reachable at its Tailscale hostname from a phone; end-to-end photo → created enriched entity works;
    architecture doc updated in the same commit.
  - Deps: P2-05, P2-06, P2-07.

## Phase 2 exit criteria

From a phone on Tailscale, snap a model sticker (and optionally its receipt), confirm the inferred fields, and get a
Homebox entity created in the right household with identity + purchase metadata, a receipt attachment, an official
product photo, fetched manuals, and a warranty estimate — without ever logging into Homebox.

## Open Phase-2 design notes

- **Household selection** is by which Homebox API token the portal uses (a token is group-scoped). Multi-household =
  multiple tokens / a token switcher — deferred (see decisions.md).
- **Multi-item receipts** (one receipt → several entities) — deferred.
- Auth on the portal itself: Tailscale-only is the baseline control; revisit if it ever needs public exposure
  (would require OIDC per homelab conventions — default is private, so likely never).
