# Pipeline accuracy — current design, assessed recommendations, backlog

Status: **working document** (iterate here; promote agreed items to D-rows in
`docs/decisions.md` and milestones in `docs/plan-architecture-v2.md`).
Scope: how docfetch decides what to attach, where that decision is weak, and
what to change — ranked by value per unit of cost, no new hardware assumed.

---

## 1. The pipeline as it stands

### 1.1 Intake (portal, `internal/portal`) — no web egress

```
photos ──┬─▶ vision model (sticker/receipt/warranty)  → manufacturer, model, serial,
         │                                              purchase block, warranty terms
         └─▶ local QR decode (EVERY photo, gozxing)   → qr.link signal events
                                                        (platform targets kept as
                                                         provenance only)
                          │
                          ▼
              entity created in Homebox + events written + scanner triggered
```

### 1.2 Curation (scanner, `internal/scheduler` + `internal/discovery`) — all egress

```
per entity (inflight-guarded, one goroutine per item)
  │
  ├─ 1. ENRICH  fill-only identity completion (manufacturer/modelNumber/name/category)
  │             gate: ≥2 agreeing domains + back-check round-trip + conf ≥ threshold
  │
  ├─ 2. DISCOVER  per doc class (manual primary; parts secondary)
  │      stages, in order:  qr → brand-site → web-pdf → web-html
  │      candidates scored by rules (model token in URL/title, official domain,
  │      PDF vs HTML, spam/re-host/marketplace blocklists, ccTLD region penalty)
  │      LLM rerank = tiebreaker only
  │
  ├─ 3. DECIDE   ladder in resolveManual():
  │      official-first  → skim-confirmed official PDF beats any non-official
  │      auto-attach     → conf ≥ auto_attach_threshold, non-HTML
  │      page-follow     → official PDF harvested from the winning HTML page
  │      skim-promote    → doc TEXT names the model → attach at 0.85
  │      link            → official remote PDF and/or support page into fields
  │      review-gate     → ntfy prompt with signed Attach/Reject
  │      notfound        → ledger + event, backoff
  │
  ├─ 4. VERIFY   Skim() on downloaded bytes: %PDF magic, model-in-text (rules),
  │              LLM SkimDoc (docType/models/differentProduct) when inconclusive
  │              → veto (wrong class / different product) or promote (model confirmed)
  │
  └─ 5. CURATE   official photo (og:image → verify, or image search → vision pick),
                 warranty estimate, tagging, breadcrumb, events
```

### 1.3 Feedback that already exists

- **Per-URL negative memory**: ntfy Reject, or deleting the artifact in Homebox
  (sweep) → `doc.reject` signal event, permanent, filtered from every future pass.
- **Decision ledger** (`decisions` table): stage, candidate set with scores,
  outcome, later labels (`confirmed`/`rejected`/`overridden`).
- **Event log** (`events` table): every scanner action, browsable at `/log`.

---

## 2. Assessment of the three recommendations

All three were checked against the code. **All three are factually correct.**

### R1 — Text extraction is the weak link ✅ confirmed, highest value

`internal/discovery/verify.go:52` calls `pdfText(data, 6, 16_000)`, backed by
`github.com/ledongthuc/pdf`. On empty extraction, `verify.go:53-57` returns a
verdict with `HasText=false`, `ModelConfirmed=false`, no mismatch flags. That
verdict passes `skimAccepts()` (`scanner.go`) — `IsPDF` is true, no veto flags
set — so **the document attaches with zero content verification**. The
`recover()` at `verify.go:130` means a parser panic produces the same silent
outcome. Extraction failure is indistinguishable from a clean read.

Value: this is the failure mode that produced the wrong-company manual link in
live testing. Every gate downstream of "we could read the text" is dark.

**Cost the original recommendation understates:** docfetch is a CGO-free static
binary on `gcr.io/distroless/static-debian12` — a locked convention (CLAUDE.md,
D1). Shelling out to `pdftotext` means:
- base image moves to `distroless/base` or `debian-slim` + `poppler-utils`
  (realistically +40–60MB, not +15MB);
- `make dev` needs `brew install poppler` on the dev machine;
- a new degradation path: binary missing → behave how?

That does not kill the idea — there is no good pure-Go alternative (pdfcpu's
extraction is weaker, go-fitz needs CGO, UniPDF is commercially licensed) — but
it is a **decision reversal that needs a D-row**, not a drop-in.

**Recommended shape:** `pdftotext` as an *optional external extractor*,
config-gated (`curation.docs.pdf_extractor: builtin|pdftotext|auto`), tried
first when present, falling back to the built-in reader. Crucially, pair it
with R4 below — an unreadable PDF must stop being an automatic pass.

### R2 — Scanned/image-only PDFs are never verified ✅ confirmed, defer

Same code path, same silent pass. OCR (`pdftoppm` + `tesseract`) would convert
"assume it's fine" into real confirmation on old-appliance scans, where URL
heuristics are weakest.

**Why defer:** ~120MB image growth on top of R1's, a second external
dependency, and per-doc latency in seconds. It is the *right* eventual answer
for a class of documents we genuinely cannot verify otherwise — but it should
land after R1+R4 are measured, opt-in (`ocr.enabled: false` default), page- and
time-capped, and scanner-only (never the portal, which must stay egress- and
dependency-light for the offline-LLM goal).

### R3 — Model scan window is too narrow ✅ confirmed, do first

`pdfText(data, 6, 16_000)` reads the first 6 pages / 16KB. "Models covered"
tables live on back covers and in mid-document spec tables. Widening to
**first 6 + last 3 pages** is pure recall gain: the bytes are already in
memory, no new dependency, no image change, ~zero latency.

This is the only one of the three that is free. It ships first.

---

## 3. What the recommendations miss

### R4 — Unverifiable ≠ verified (the actual bug behind R1/R2)

Even with perfect extraction, the *policy* is wrong: `HasText=false` currently
means "benefit of the doubt, attach anyway". It should mean "inconclusive" and
route by provenance:

| Source | Text unreadable | Proposed |
|---|---|---|
| Official brand domain | attaches today | **keep attaching** (provenance is the evidence) |
| QR-linked | attaches today | **keep attaching** (physical-label provenance) |
| Non-official (aggregator, re-host, search result) | attaches today | **review-gate instead** |

This is a few lines in `skimAccepts()` + `resolveManual()`, costs nothing, and
removes the whole class of "silently unverified attach" *independently* of how
good extraction gets. It also makes R1's benefit measurable: fewer review
prompts = extraction actually improved.

### R5 — No way to measure any of this (top priority)

Every recommendation above is currently unfalsifiable: there is no accuracy
number to move. We have the raw material — `decisions` rows carry the full
candidate set with scores, `events` carry outcomes and user labels — but no
harness that replays them.

**Golden-set replay** (already backlogged as learning Phase B, plan M5) should
be pulled forward *ahead of R1/R2*: freeze ~20–30 real items with known-correct
answers, replay the recorded candidate sets through the current scorer, report
precision/recall per stage. Without it, R1 and R2 are expensive guesses; with
it, each becomes a measurable delta and a regression test.

### R6 — Weak-identity items auto-attach

The entire verification stack keys off `ModelNumber`: `skimPromote()` returns
early when it is empty (`scanner.go:501`), and `modelInText()` needs ≥4 chars.
An item with no model number can still reach auto-attach on rules + rerank
score alone. Proposal: when identity is weak (no model number, or only a
generic name), cap the outcome at **link or review-gate** — never auto-attach.
Cheap, and matches the trust model already used for metadata writes.

### R7 — Photo curation has an asymmetric gate

`curatePhoto()` verifies the og:image path with `VerifyProductImage`
(`curate.go:121`) but the image-search path attaches on `PickProductImage`
confidence alone (`curate.go:157`). A ranker's "best of 5" is not the same
claim as "this is the product" — that asymmetry is what let a stock photo
attach at 0.9 before the host blocklist caught it. Run the verify call on the
winner of the search path too: one extra small vision call per photo.

### R8 — Extraction failures are invisible

`pdfText` returns `""` for "no text", "parse error", and "panic" alike, with no
event. Add a `skim.unreadable` event (with the reason) so the log shows how
often verification is actually running. This is the cheapest possible
instrumentation for R1's value case — run it for a week *before* changing the
image, and we will know whether extraction failure is a 5% or a 40% problem.

---

## 4. Proposed sequence

| # | Change | Cost | Risk | Gate |
|---|---|---|---|---|
| **A1** | R3 widen scan window (first 6 + last 3 pages) | ~0 | none | ships now |
| **A2** | R8 `skim.unreadable` event + extraction-reason logging | ~0 | none | ships now |
| **A3** | R4 provenance-aware handling of unreadable PDFs | ~0 | low | ships now |
| **A4** | R6 weak-identity cap (no auto-attach without model evidence) | ~0 | low | ships now |
| **A5** | R7 verify the image-search photo winner | 1 vision call/photo | low | ships now |
| **A6** | §5.4 verbatim evidence strings per extracted field + confirm-screen hints | prompt/schema only | none | ships now |
| **B0** | §5.3 `bench-vision` harness + vision-model bake-off | ~½ session | none (offline) | before B1 |
| **B1** | R5 golden-set replay harness (learning Phase B) | ~1 session | none (offline) | before B2 |
| **B2** | R1 `pdftotext` optional extractor + image base change | image +40–60MB, dev dep, D-row | medium | after A2 data + B1 baseline |
| **C1** | R2 OCR for scanned PDFs, opt-in, capped | image +~120MB, seconds/doc | medium | after B2 measured |

Everything in tier A is free, independent, and shippable in one pass. Tier B
changes the packaging contract and should be justified by A2's numbers and
measured by B1 — except **B0, which should come first of the lot**: it is the
cheapest harness, it fixes the root of the dependency chain (identity), and its
result (a better vision model) changes what every later measurement sees. Tier
C is a real feature with a real cost — worth it for old appliances, not worth
it blind.

---

## 5. Model selection and evidence visibility

### 5.1 There is no training — say "evidence", not "training"

Nothing in docfetch is fine-tuned or trained. What exists is a **labeled
ledger**: decisions with candidate sets and scores, plus permanent per-URL
negative memory from user rejections. Model behavior changes only when the
model *name in config* changes. This matters for planning: "improve the
training" is not an available lever; "pick a better model" and "show the human
why the machine believed something" are.

### 5.2 The two LLM roles have opposite economics — and the budget is backwards

| Role | Model today | Calls | Leverage |
|---|---|---|---|
| **Intake vision** (`ExtractIntake`) | `gemini-2.5-flash-lite` (cheapest tier) | **once per item**, user-initiated | **highest** — a wrong model number poisons every downstream stage: search queries, `modelInText`, `skimPromote`, photo subject, warranty subject |
| **Rerank / skim** (`SkimDoc`, candidate rerank) | `llama-3.1-8b-instruct` | many per scan, per class, ongoing | moderate — rules-first design means it breaks ties, but its confidence feeds `auto_attach_threshold` (the Ecowitt wrong-company PDF scored 0.9 here) |

Intake vision runs a handful of times a month on a homelab and is currently on
the *cheapest available* tier; rerank runs constantly and dominates the ~$5/mo
budget. Upgrading vision is bounded, cheap, and sits at the root of the
dependency chain. **That is where to spend first.** Rerank is where to spend
*carefully*, since volume multiplies any price increase.

### 5.3 Can we just pick the best model? No — but the testbed is cheap here

Public leaderboards and "best OCR model 2026" listicles do not answer *our*
question: how well does a model read **a curved, glare-hit sticker photographed
by a phone at an angle**. That is a narrow task with idiosyncratic failure
modes (serial-vs-model confusion, FCC IDs, part numbers, revision suffixes).

The good news: unlike the doc pipeline, **intake vision is a pure function** —
photo bytes → JSON. No web egress, no ordering, no state. A model bake-off is
therefore the *easiest* harness in this project:

```
testdata/intake/<case>/{photo.jpg, expected.json}
docfetch bench-vision --models a,b,c   →  per-model, per-field accuracy + cost
```

20–30 real photos (which we generate simply by using the portal) with
hand-checked expected values. Run N models over the same set, diff per field,
report exact-match on `modelNumber`/`manufacturer` plus cost per run. This is
the same golden-set idea as B1, applied to a much simpler surface — and it
should ship *first* because it is easier and its result changes what everything
else sees.

Candidates worth putting in the first bake-off (as of Aug 2026 — verify current
availability/pricing on OpenRouter, do not trust listicles):
current-generation Gemini Flash (a straight upgrade from `flash-lite`),
Qwen3-VL sizes, and a purpose-built OCR model such as Qianfan-OCR-Fast for the
sticker case specifically. The harness picks the winner, not the leaderboard.

**Recommendation:** build `bench-vision` (half a session), run it, then change
`llm.vision_model` on evidence. Repeat later for rerank using recorded
`decisions` rows (B1) — that one needs the replay harness because rerank
quality is only meaningful in the context of a candidate set.

### 5.4 Evidence visibility: mark the *text*, not the image

The ask — "mark the images with what was identified" — is directionally right
(a human should be able to check the machine's reading) but the bounding-box
implementation is the expensive version of it:

- vision models via OpenRouter return coordinates unreliably across model
  families; the annotation would be wrong exactly when the extraction is wrong;
- it needs an image-processing step, a re-encode, and a *second derived image*
  stored somewhere — either polluting the Homebox attachment list with machine
  artifacts, or building new blob storage docfetch does not currently have.

The cheap version delivers the same human value: **verbatim source text +
per-field confidence**. Extend the intake extraction schema so every field
carries what the model *actually read*:

```json
"modelNumber": "WDF520PADM7",
"evidence": { "modelNumber": "MOD. WDF520PADM7", "manufacturer": "Whirlpool®" }
```

That is a prompt + schema change (no new dependency, no image handling) and it
catches the exact failure it needs to catch: if the model grabbed the serial,
the evidence string reads `S/N: F82304891` and the error is obvious at a
glance. Surface it in two places:

1. **Confirm screen** — a dim monospace hint under each field: `read as “MOD.
   WDF520PADM7” · 0.92`. The human is already reviewing these values there;
   this is where a bad read gets caught before it poisons anything.
2. **Event log** — `intake.created` detail gains the evidence map, so
   `/log/{id}` explains where identity came from long after intake.

Gate behind `intake.show_evidence` (default on — it costs nothing and this is
the whole point of a human-in-the-loop intake).

**Verdict on image marking: overkill *for now*.** Revisit only if the evidence
strings prove insufficient — e.g. dense multi-label stickers where knowing
*which* of four numbers was read still isn't clear from text alone. The
evidence-string version should be built first and will likely settle it.

- **Community/official maintenance resources** (plan M6): repair videos,
  simple-fix guides; non-promotional gating is the hard part. QR platform
  targets are already being recorded as raw material.
- **Learned domain priors / threshold calibration** (learning Phases C–D):
  deferred until B1 shows thresholds are actually miscalibrated on real data.
- **Shared community priors file** (adblock-list model: curated
  `domain → trust` deltas shipped as data, no telemetry).
- **Per-class thresholds**: one global `auto_attach_threshold` governs classes
  with very different risk profiles (a wrong manual is worse than a wrong parts
  list).
- **Region/language gates**: current ccTLD penalty is generic; per-brand market
  knowledge (some makers only publish EU-market PDFs) is unmodeled.
- **Re-verification on demand**: no way to say "re-check this item's manual"
  short of deleting the attachment. A portal action would help testing.
- **Multi-item receipts**, **multi-household**, **classic-Homebox adapter** —
  tracked in `docs/decisions.md` backlog, not accuracy work.
