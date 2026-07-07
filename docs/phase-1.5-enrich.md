# Phase 1.5 — Metadata enrichment

Deliverable: the scanner auto-completes item identity metadata (manufacturer / modelNumber / name /
category) with high confidence before doc-fetch runs, so name-only items become model-anchored and
manual search precision improves.

**Prereq:** Phase 1 deployed (done 2026-07-06). Read [spec.md](spec.md) §6 — especially the
PATCH-ignores-scalar-metadata gotcha: **all metadata writes go through `PutEntity` (fetch → merge → PUT)**.

## Design rules (locked — see decisions.md D13–D16)

- **Fill-only.** Machine writes only empty fields; human-entered values are never overwritten.
- **Per-field gating.** Each field has its own confidence; one field can write while another goes to review.
- **Confidence = corroboration, not LLM self-score.** A field is writeable only if
  (a) extracted value agrees across ≥ `min_agreeing_sources` independent domains, and
  (b) the back-check round-trip passes (search the inferred identity → results mention the original name/model).
- **Provenance.** Every write appends a `docfetch:` line to `notes` (fields, evidence URL, confidence)
  and keeps/applies the `docfetch/unverified` tag until a human blesses it. Full audit in SQLite.
- **Enrich runs before doc-fetch** in the same scan pass; a filled model# immediately upgrades the
  manual query from name-based to model-anchored.

## Pipeline

```
enrich(item with gaps):
  key    = modelNumber if present else name          (strongest available key)
  search = SearXNG "<key> <manufacturer?>"           (1-2 queries)
  extract= LLM over titles/urls/snippets ->
           {manufacturer, modelNumber, name, category, per-field confidence, evidence[]}
  validate per field:
    - >=2 independent domains agree on the value
    - back-check: search inferred "mfr model" -> top results mention original name (or vice versa)
    - format sanity on modelNumber (has digits/dashes, not a marketing word)
  gate per field:
    pass            -> stage write
    LLM-high only   -> ntfy review ("docfetch: confirm metadata for X")
    low             -> skip; retry on followup cadence
  write staged fields via PutEntity (fetch->merge; fill-only) + provenance note + record in SQLite
```

## Config (additions)

```yaml
enrich:
  enabled: true
  fill_only: true
  auto_write_threshold: 0.85
  min_agreeing_sources: 2
  back_check: true
  fields: [manufacturer, modelNumber, name, category]
  provenance_note: true
```

`category` maps to a Homebox tag (create-if-missing), not a scalar field.

## State (additions)

New SQLite table `enrichments`:
`entity_id, field, value, confidence, evidence_urls, written_at, superseded` — audit + undo +
"don't re-ask" memory. An item is re-enriched only when its meta_hash changes or on followup after
`backoff_base` (same discipline as doc-fetch).

---

## Tasks

- [x] **P1.5-00 · Store: enrichments table**
  - Intent: migration + CRUD for the audit table; helper `AlreadyEnriched(entity, field)`.
  - Files: `internal/store/`.
  - Acceptance: migrate on boot (existing DBs upgrade cleanly); upsert/query; survives restart.
  - Deps: none.

- [x] **P1.5-01 · LLM extraction call**
  - Intent: `llm.ExtractIdentity(itemDesc, candidates) -> {fields, perFieldConf, evidence}`.
    Same cost discipline as Rerank: snippets ≤150 chars, constrained JSON out, ~80 max_tokens.
  - Files: `internal/llm/`.
  - Acceptance: live test — name-only "PlayStation Portal" extracts Sony + CFI-Y1000-class model;
    model-only "WH-1000XM5" extracts Sony + headphones name. Strict JSON.
  - Deps: none.

- [x] **P1.5-02 · Enrich engine (validate + gate)**
  - Intent: `internal/enrich`: search→extract→cross-validate (domain agreement, back-check round trip,
    model# format sanity)→per-field gate. Injected interfaces (searcher/LLM) for hermetic tests.
  - Files: `internal/enrich/`.
  - Acceptance: unit tests — agreement counting across domains, back-check pass/fail, fill-only staging,
    per-field gate splits (one writes, one reviews).
  - Deps: P1.5-01.

- [x] **P1.5-03 · Writer: PutEntity merge + provenance**
  - Intent: fetch current entity → merge staged fields (fill-only) → PUT; append provenance note to
    `notes`; ensure `docfetch/unverified` tag; record rows in `enrichments`.
  - Files: `internal/enrich/`, `internal/homebox/`.
  - Acceptance: live — created test item gains fields via PUT without clobbering existing values;
    note present; audit row written; re-run is a no-op.
  - Deps: P1.5-00, P1.5-02.

- [x] **P1.5-04 · Scanner integration + review notifications**
  - Intent: enrich step runs before doc-fetch in `process()` when identity gaps exist; mid-confidence
    fields send one ntfy ("confirm metadata"); config block parsed + validated; example/ansible templates updated.
  - Files: `internal/scheduler/`, `internal/config/`, `config.example.yaml`, `ansible/roles/docfetch/templates/config.yaml.j2`.
  - Acceptance: unit test — name-only item enriches then doc-fetches with model-anchored query in one pass;
    disabled flag bypasses cleanly; ntfy sent once per pending field-set.
  - Deps: P1.5-02, P1.5-03.

- [x] **P1.5-05 · Deploy + live validation**
  - Intent: redeploy role; live e2e — name-only item gets mfr/model auto-filled + manual attached in one scan;
    docs/memory updated (spec §5 config, decisions, this board, project memory).
  - Files: role, docs, memory.
  - Acceptance: live item enriched with provenance + manual attached; suite green; board marked.
  - Deps: P1.5-04.

## Exit criteria

A name-only item added to Homebox gets manufacturer + model number (and category tag) auto-filled with
corroborated confidence, a provenance note, and a model-anchored manual attached — in a single 15-min
scan cycle — while human-entered fields are never touched and every machine write is auditable and undoable.
