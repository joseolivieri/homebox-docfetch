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

## Open questions (BLOCKING)

| # | Question | Blocks | Default if unanswered |
|---|---|---|---|
| Q1 | **Homebox API token** — provide a scoped token (Homebox → user settings), or walk through creating one. Needed to also pin the live `entityTypeId` for "Item" and confirm attachment behavior. | P1-02 (client), all live testing | — none; hard blocker |
| Q2 | **OpenRouter key** — reuse the Norish/Sparky key, or a **separate key** to isolate this $5 budget? | P1-05 (rerank), P2 vision | Recommend separate key (clean cost attribution). |
| Q3 | **Scheduler confidence gate** — confirm **review-gate via ntfy** (low-confidence → notify, don't attach) vs auto-attach best guess. | P1-06 (gate) | Recommend review-gate. |

## Deferred / future (not in scope now)

- Multi-item receipts (one receipt → parse line items → create N entities).
- Multi-user / multi-household (each = a Homebox group = a separate scoped token).
- Evaluating `/attachments/external` if Homebox later gains true URL-fetch semantics.
