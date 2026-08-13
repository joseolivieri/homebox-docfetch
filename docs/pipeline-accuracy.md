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
| **B1** | R5 golden-set replay harness (learning Phase B) | ~1 session | none (offline) | before B2 |
| **B2** | R1 `pdftotext` optional extractor + image base change | image +40–60MB, dev dep, D-row | medium | after A2 data + B1 baseline |
| **C1** | R2 OCR for scanned PDFs, opt-in, capped | image +~120MB, seconds/doc | medium | after B2 measured |

Everything in tier A is free, independent, and shippable in one pass. Tier B
changes the packaging contract and should be justified by A2's numbers and
measured by B1. Tier C is a real feature with a real cost — worth it for old
appliances, not worth it blind.

---

## 5. Accuracy backlog (beyond the above)

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
