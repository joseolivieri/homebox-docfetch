# Architecture v2 — planning document

Status: **agreed 2026-07-16** (open questions resolved — see §8; D-rows land in
decisions.md as milestones start). Scope: replace the notes-field bus with a shared event
store, collapse the two-container deployment, and define the baseline for a publicly
releasable homelab service.

---

## 1. Problem statement

Three pressures on the current architecture:

1. **The notes bus hit its ceiling.** Homebox's ~1000-char notes cap forces a budget/prune
   dance; every notes write bumps `updatedAt` and self-triggers the change-poll (worked
   around with dedupe); an oversized PUT 500s and loses the whole payload; semantic lines
   (`qr`/`rejected`/`approved`) are parsed with keyword heuristics that already needed
   word-boundary fixes. The bus was a good bootstrap; it is now the most fragile part of
   the system.
2. **Two containers run the same binary in two modes** and both call the same `build()`
   path — the portal container even opens the sqlite store and never uses it. The split
   buys container-level egress isolation and costs everything else: two lifecycles, no
   shared state, notes-as-IPC.
3. **Public release** raises requirements the current single-household design never had:
   setup variance (search engine, LLM endpoint, notifier, ingress), a migration story for
   durable state, and — the elephant — the Homebox *entity-model fork* dependency.

## 2. Assessment of the proposal (challenged point by point)

### 2.1 "Portal and scanner should share the DB" — agree, but reframed

The problem is not storage, it is **transport**: portal→scanner signals currently ride an
external, size-capped, self-triggering channel. Alternatives considered:

| Option | Verdict |
|---|---|
| Keep notes bus | No. Limits documented above are structural, not tunable. |
| Homebox custom fields as bus | No. Same PUT-full-replace hazards, same self-trigger. |
| Shared sqlite volume across two containers | **No.** Multi-process sqlite writers across container boundaries is the worst of all worlds (locking, WAL over shared volume). If we share the DB, we must share the process. |
| Portal→scanner internal HTTP API | Works, but invents an API between two halves of the same binary. Overkill. |
| **Single process, shared store** | Yes. One writer, no IPC, transactional handoff. |

So: shared DB is correct **because** we merge into one process (2.4), not independently of it.

What the DB bus wins immediately:
- No notes budget/prune; no PUT-500-loses-everything exposure from our own writes.
- **Kills the self-trigger class of bugs** — docfetch stops writing notes, so its own
  writes no longer bump `updatedAt` (review-gate dedupe stays as belt-and-braces).
- `qr`/`rejected`/`approved` become indexed queries instead of regex over prose.
- Container restart no longer re-primes the change-poll into eating pending signals
  (cursor persisted in DB — fixes a documented operational gotcha).

**Honest cost — state becomes precious.** Today, rejection memory lives in Homebox and
survives loss of the docfetch volume. After v2, deleting the state volume forgets
rejections/approvals and re-attaches previously rejected docs. Mitigations: document the
volume as durable state; optional Litestream/backup guidance; keep the one-line breadcrumb
(2.3) so a human can still see something happened. This tradeoff is accepted, not hidden.

**What stays in Homebox** (unchanged semantics): attachments, fields, tags, and the
user-intervention path "delete the attachment ⇒ retry now" — that is Homebox-native and
correct. Homebox remains the source of truth for *inventory*; the docfetch DB owns
*process state and audit*, which it already half-does (items/decisions/enrichments tables).

### 2.2 "Jobs that aggregate actions/learnings/audits" — challenge: no jobs

Batch aggregation jobs are the wrong shape. Every action already flows through a decision
point in code; write an **append-only event row synchronously at that point** and there is
nothing to aggregate later — aggregation is a read-time query. Jobs would add drift
(source-of-truth ambiguity between raw actions and aggregates) and a scheduling surface
for zero benefit at this data volume. The only periodic task needed is retention pruning.

Proposed schema (generalizes — does not replace — the existing `decisions` table, which
stays as the ML-oriented ledger per D17):

```sql
CREATE TABLE events (
  id        INTEGER PRIMARY KEY,
  ts        TEXT NOT NULL,             -- RFC3339
  entity_id TEXT NOT NULL,
  actor     TEXT NOT NULL,             -- scanner | portal | user | system
  kind      TEXT NOT NULL,             -- intake.created, qr.link, enrich.write,
                                       -- doc.attach, doc.reject, doc.approve,
                                       -- photo.attach, warranty.set, sweep.removed,
                                       -- review.request, error
  class     TEXT,                      -- manual | parts | photo | warranty | ...
  url       TEXT,
  detail    TEXT                       -- JSON payload (scores, confidence, filenames)
);
CREATE INDEX events_entity ON events(entity_id, ts);
CREATE INDEX events_kind   ON events(kind, ts);
```

`notes.RejectedURLs/ApprovedURLs/QRURLs` become `SELECT url FROM events WHERE entity_id=?
AND kind=?`. Migration: one-time importer parses existing notes semantic lines into events,
then docfetch stops writing notes (except the optional breadcrumb).

### 2.3 "Cheap way to see the log" — agree; three cheap surfaces

1. **Portal pages** (the portal is already an HTTP server): `/log` — recent events across
   all items; `/log/{entityID}` — full per-item history. Server-rendered HTML, no JS
   framework, reuses the existing portal handler/templating. This is the primary surface.
2. **CLI**: `docfetch log [--entity ID] [-n 50]` against the same store — works in
   `docker exec` for headless debugging.
3. **Optional Homebox breadcrumb** (config: `notes.breadcrumb: true|false`): a single
   stable line, e.g. `docfetch: manual ✓ parts ✓ photo ✓ — log: <portal>/log/{id}`,
   rewritten idempotently (same dedupe discipline as today's tagging). Default **on**:
   the notes log's real value was visibility where the user lives (Homebox UI on the
   phone); one line preserves that at 1/10 the byte cost. Zero notes writes remains
   available for purists.

### 2.4 "Single container for our custom code" — agree; kill split mode rather than half-keep it

Merge is nearly free: both modes share `build()` today. Add a `serve` subcommand that runs
the cron scheduler and the portal HTTP server in one process on the shared store.

Deployment becomes **two containers total**: `docfetch` (serve) + `searxng`. SearXNG stays
separate — it is upstream software, its own lifecycle, and swappable per §4.1.

**What we lose and accept:** container-level egress isolation (today the portal container
can be firewalled to homebox+LLM only). The intake/curation separation was always enforced
primarily in code — `internal/portal` imports no discovery/egress packages — and that
compile-time guarantee survives the merge. For a homelab audience, one container with a
code-enforced boundary beats two containers with a fragile IPC channel. The
"offline-LLM-ready" property is a code-path property, not a container property.

**Decisive cut:** deprecate standalone `portal` and `scheduler` subcommands (keep `once`,
`probe` as tools). Keeping split mode alive would require the DB bus to also work
cross-process — i.e., keeping the notes bus as a fallback — which means two code paths
forever. One blessed deployment shape is a feature for a public project, not a limitation.
(Split-host was considered and rejected — §8 Q1.)

## 3. The challenge back: what the proposal underweights

### 3.1 Homebox target: upstream entities (resolved 2026-07-16)

Original concern: docfetch targeted the entity-model *fork*, and a public release against
a fork is a release for an audience of one. **Resolved by events:** upstream Homebox
shipped the entity overhaul in June 2026 — the entity model is now upstream's latest.
Decision: track upstream's entity API as the sole target; a legacy (pre-entity) adapter is
built only if real demand appears post-release. The old "classic-Homebox adapter" backlog
item is retired in favor of that demand-gated stance.

Residual risk this creates: **fork ↔ merged-release drift.** docs/spec.md was derived
from the fork; the API that upstream actually shipped may differ in details (paths, field
names, attachment `type` enum, parentId semantics, notes length cap). A **parity audit**
against the June upstream release is required work (M3): run `docfetch probe` + the
golden-path live test against an upstream instance, diff behavior, update spec.md to cite
upstream as authority. The `EntityAPI`/InventoryBackend seam (§4.3) stays — now as
insulation against upstream API evolution rather than as a multi-backend layer.

### 3.2 LLM endpoint variance matters more than search engine variance

The proposal asks for a search-provider layer; fine (§4.1). But for homelab users the
sharper variance is the **LLM endpoint**: many will refuse a cloud key and want
Ollama/llama.cpp/LM Studio. We are already OpenAI-compatible via base_url (OpenRouter),
so this is mostly *testing + documentation + graceful degradation*, not new code:

- Verify against Ollama's OpenAI-compatible endpoint (rerank + vision with e.g.
  qwen2.5vl); document model recommendations per VRAM tier.
- Degradation matrix already half-exists (nil reranker ⇒ rules-only): make explicit and
  tested — no LLM = rules-only discovery, no enrichment, no vision intake; small local
  model = full pipeline with lower thresholds.

### 3.3 "Pattern learning models" — premature; sequence it behind data

Learned domain priors / threshold calibration (phases C–D) on one household's data volume
would overfit noise. The correct order is already in the backlog: **Phase B (golden-set
replay) first**, and the v2 event store is precisely what makes it feasible — decisions +
events give labeled replay data for free. Phase B lands as M5; C–D stay deferred until
replay shows thresholds actually miscalibrated. A community-shared priors file
(adblock-list model: curated `domain → trust` deltas shipped as data, not telemetry) is
noted as a future idea compatible with homelab privacy expectations — no phoning home.

### 3.4 "Incorporate pre-built services" — mostly no; each dependency is setup variance

The stated goal (account for setup variance) argues **against** adding services. Position:

| Service | Verdict |
|---|---|
| SearXNG | Keep as default, bundled in compose. Becomes one provider behind the seam (§4.1). |
| FlareSolverr (bot-blocked hosts) | Optional hook, config-gated, off by default. Real benefit for exactly the hosts we blocklist today; zero cost when absent. |
| LiteLLM proxy | Don't bundle; OpenAI-compatible base_url means users who want it just point at it. Document. |
| Apprise | Don't add a container. Replace/augment `internal/notify` with a shoutrrr-style multi-provider URL (§4.2) — ntfy stays default, Discord/Telegram/etc. come free in-binary. |
| Meilisearch / vector DBs / queues | No. sqlite + cron is the right weight class; a queue for <100 items/day is résumé-driven design. |

Core principle: **zero-dependency core** (one static binary + sqlite volume), optional
enhancers. That is the setup-variance story.

### 3.5 sqlite stays — pre-empting the Postgres question

Single writer, embedded, CGO-free, one-file backup, no service to operate. Every "should
we support Postgres for multi-user" instinct is answered "no" until multi-household is
real (it is explicitly backlogged). Baseline discipline it *does* demand for public
release: **versioned schema migrations** (numbered, embedded, forward-only — the current
`CREATE TABLE IF NOT EXISTS` pattern stops scaling the moment external users hold data we
must not corrupt).

## 4. Provider seams (the "standard layers")

Formalize three seams. All three already exist embryonically; this is promotion to
interface + registry, not invention. Per the proposal-pattern backlog note, the trigger
condition ("first new curation source") is met by this plan.

### 4.1 SearchProvider

```go
type SearchProvider interface {
    Search(ctx context.Context, q string, opt SearchOpts) ([]SearchResult, error)
}
```

- `searxng` (default, bundled) — current code, moved behind the interface. **Only
  implementation shipped at M3**; the seam is proven by tests, not by a second provider
  nobody asked for.
- First hosted implementation when demand appears: **Brave Search API** (free tier
  ~2k queries/mo covers homelab volume, independent index, clean ToS). Kagi is paid-only;
  Serper-class SERP resellers are ToS-murky — rejected.
- Explicitly not: scraping DuckDuckGo/Google (brittle, ToS-hostile), five providers on
  day one. Config: `discovery.provider: searxng` (extensible), per-provider block.

### 4.2 Notifier

Replace ntfy-only client with a provider-URL scheme (shoutrrr or equivalent ~small dep):
`notify.url: ntfy://…` default; Discord/Telegram/Slack/webhook free. The review-gate
action links (HMAC-signed approve/reject) are notifier-agnostic already — they point at
the portal.

### 4.3 InventoryBackend (the Homebox seam)

Current `EntityAPI` interface, hardened. Single implementation: upstream Homebox entity
API (post-June-2026). Purpose: insulation against upstream API evolution and the landing
zone for a demand-gated legacy adapter later (§3.1). No capability-map machinery until a
second backend actually exists.

LLM stays "any OpenAI-compatible endpoint" — a base_url is already the abstraction; no
new interface needed (§3.2).

## 5. Production-readiness baseline (definition, so "production ready" is checkable)

- [ ] Versioned schema migrations, embedded, run at startup.
- [ ] `/healthz` (liveness) and `/readyz` (Homebox reachable, store writable) on the portal listener.
- [ ] Prometheus `/metrics` (optional flag): scans run, docs attached, LLM tokens/cost, egress calls.
- [ ] Structured logs (slog, JSON option), event log as the user-facing audit surface.
- [ ] Config validation with actionable errors + `docfetch check` (config + connectivity dry-run).
- [ ] Minimal config: `homebox.url`, `HOMEBOX_TOKEN`, optionally `OPENROUTER_API_KEY` — everything else defaulted. Current config.example.yaml becomes the *full reference*, a 15-line quickstart config becomes the front door.
- [ ] Graceful shutdown (finish in-flight entity, checkpoint cursor).
- [ ] Docker: distroless static binary (have), non-root (verify), single volume `/data` (db + cache), healthcheck directive, GHCR multi-arch (add arm64 — homelab = Pi/N100 crowd).
- [ ] Backup note: `/data` is durable state; document sqlite backup / Litestream option.
- [ ] Docs: quickstart (compose up), setup-variance matrix (search × LLM × notify × Homebox variant), threat notes (egress, tokens).

## 6. Milestones

| # | Scope | Risk |
|---|---|---|
| **M1** ✅ (D25) | `serve` subcommand (scheduler + portal, one process); compose collapses to docfetch+searxng; deprecate split subcommands; arm64 image. No behavior change otherwise. | Low |
| **M2** ✅ (D26/D27) | `events` table + synchronous event writes at all decision points; DB replaces notes bus (qr/rejected/approved) with portal→scanner trigger; idempotent notes importer (transition); breadcrumb line (default on); portal `/log` pages + `docfetch log`; change-poll cursor persisted. | Medium — touches scanner/portal handoff; gated by golden-path live test |
| **M3** | Provider seams: SearchProvider (searxng only), notifier URL scheme, Ollama-verified LLM path + degradation matrix; **upstream entity-API parity audit** (§3.1) + spec.md update; migrations framework; healthz/metrics/check; config minimization. | Low-medium |
| **M4** | Public-release prep: quickstart docs, setup-variance matrix, semver v1.0 tag, backup guidance, README for external users. **Release gate.** | Low |
| **M5** | Learning Phase B: golden-set replay harness off events+decisions. C–D remain deferred. Legacy-Homebox adapter only if demand materializes. | Low |
| **M6** | **Modern resources**: official/community maintenance & repair videos, simple-fix guides — curated non-promotional links per item (new resource classes beside the doc classes; video links live in custom fields, not attachments). Raw material already accumulating: qr.link events record platform targets (a maker's YouTube channel) as provenance even though the doc pipeline skips them. Design constraints to honor while building earlier milestones: resource classes stay config-driven like doc classes; provenance tiers (official channel > community) mirror the doc trust model; promotional content is a veto class. | Medium — quality gating for community content is the hard part |

M1 and M2 are separable commits/PRs; M2 is the architectural payload.

## 7. Decision candidates (become D-rows on sign-off)

- D25: Single-process `serve` deployment; split subcommands deprecated; stage separation enforced at package level, not container level; no split-host support.
- D26: sqlite event store replaces the notes bus; notes reduced to a one-line breadcrumb (default on); docfetch state volume declared durable, backups documented (no Homebox-side rejection mirroring).
- D27: Aggregation is read-time over append-only events; no batch aggregation jobs; retention prune only.
- D28: Zero-dependency core + optional enhancers (FlareSolverr, hosted search) as the setup-variance policy.
- D29: Provider seams: SearchProvider (searxng-only at first; Brave first when demanded), notifier URL scheme, InventoryBackend; LLM remains base_url-only.
- D30: Target upstream Homebox entity API (June 2026 release); legacy adapter demand-gated; parity audit of spec.md against upstream required (M3).

## 8. Open questions — resolved 2026-07-16

1. **Split-host need:** No. Single process only.
2. **Breadcrumb default:** On.
3. **Homebox target:** Upstream shipped the entity overhaul in June 2026 — track upstream's latest, entities only. Legacy adapter only if enough demand later. (Replaces the old "classic adapter as release gate" framing; see §3.1.)
4. **Rejection durability:** No Homebox-side mirroring. Document backups.
5. **Hosted search reference:** Brave, when demand appears (free tier, independent index, clean ToS; Kagi paid-only, SERP resellers ToS-murky). M3 ships the seam with searxng only.
