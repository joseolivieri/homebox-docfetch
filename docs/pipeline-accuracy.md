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

## 6. Intake observations: more photos, staging, and a per-photo data object

Three separable ideas. They are worth very different amounts.

### 6.1 Arbitrary extra photos — trivial, do it

The four slots are a **UI convention, not a server constraint**: `handleExtract`
already QR-decodes every field in `r.MultipartForm.File` generically. Adding an
"extra photos" affordance (N un-slotted images, decode-only) is a portal-UI
change plus a decision about what happens to the bytes afterwards (§6.2).

Worth distinguishing two kinds of photo, because it drives everything else:

| Kind | Examples | Long-term value |
|---|---|---|
| **Evidence** | sticker, receipt, warranty panel, product shot | keep — re-readable, re-runnable against a better model, and the user's own record |
| **Transport** | "there's another QR on the back", packaging panels | **none once decoded** — the payload is the value, the pixels are not |

### 6.2 Staging storage — yes, but not for the stated reason

The premise ("too many photos to keep permanently in Homebox") is only half
right: transport photos shouldn't be *stored* at all, permanently or otherwise
— they should be decoded and dropped. That alone needs zero new storage, since
the client simply never re-sends them to `/api/create`.

The **actual** case for an intake staging store is different, and it already
applies today with four photos:

1. **Double upload.** The flow is stateless: the client POSTs photos to
   `/api/extract`, then POSTs *the same photos again* to `/api/create`. At 3–5MB
   per phone photo that is 25–40MB re-uploaded per intake once extra shots
   exist. Staging turns the second call into a list of IDs.
2. **Bench corpus.** B0 (`bench-vision`) needs real sticker photos with
   hand-checked expected values. Real intakes are exactly that corpus — but
   only if the originals survive long enough to be exported.
3. **Re-extraction.** Changing `llm.vision_model` should let a user re-read an
   item's sticker without re-photographing it.

Shape: `/data/intake/<session>/` on disk (**not** sqlite BLOBs — the state DB is
declared precious and backed up; multi-MB photos do not belong in it), TTL
swept by the existing reconcile job, `intake.staging_ttl: 24h`. Filesystem +
TTL, no new dependency, no GC subtlety beyond "delete old directories".

Only evidence photos then attach to Homebox on create; transport photos expire
from staging having never been stored anywhere permanent.

### 6.3 The per-photo data object — strongest idea here, but rank the signals

An `intake.observed` event per photo, carrying everything mechanically
extractable, fits the existing event model and directly serves the "show the
human what was found" goal from §5.4. But the listed signals differ by an order
of magnitude in value:

| Signal | Cost | Value for doc-fetching | Verdict |
|---|---|---|---|
| **Barcode / GTIN** (UPC-A/E, EAN-8/13, Code128/39/93) | **zero new deps** — gozxing (already in go.mod) ships `oned.NewMultiFormatUPCEANReader`; same decode pass as QR | **highest** — a canonical, deterministic product identifier beats a fuzzily-OCR'd model number, and unlocks product-database lookup as a new identity source | **do first** |
| **FCC ID** (e.g. `2AB3C-XYZ123`) | text; rides the vision call already being made | **high for electronics** — maps to the FCC's public filing database, whose filings frequently *include the manual and internal photos*. A document source, not just an identifier | do |
| **Country of origin** ("Made in China") | text, same call | low–moderate — weak input to the region/market bias already in the backlog; says nothing about which market the *manual* targets | cheap, take it |
| **Certification marks** (CE, UKCA, EAC, UL, RoHS/WEEE) | symbol recognition — the vision model must assert them | **lowest** — that an item bears CE tells the pipeline nearly nothing about where its manual lives. This is *inventory/compliance* metadata, not a discovery signal | nice-to-have; justify as an inventory feature, not an accuracy one |

Two caveats so barcodes are not oversold:

- A retail barcode identifies the **SKU/package**, which is not always the
  model number — bundles, regional SKUs and retailer-specific packs diverge,
  and the code may be on the *box* rather than the product. Treat GTIN as a
  strong *additional* identity key, never a replacement for the model number.
- Turning a GTIN into a product identity needs an **external product database**
  (GS1, UPCitemdb, Open Product Data). That is a new curation source — which,
  per the backlog rule, is the trigger for doing **provider standardization**
  first.

**Proposed shape** (matches §5.4's evidence direction):

```json
// intake.observed event, one per photo
{ "slot": "extra-2",
  "qr":      ["https://acme.example/support/wt41"],
  "barcode": [{"format": "EAN13", "value": "0712345678901"}],
  "text":    {"fccId": "2AB3C-XYZ", "originCountry": "Thailand"},
  "marks":   ["CE", "UKCA"] }
```

Decode (QR + barcode) is deterministic and local — it stays inside the intake
egress boundary. `text`/`marks` ride the existing vision call as extra schema
fields, costing no additional request. The object renders on the confirm screen
and in `/log`, which is the human visibility asked for in §5.4.

### 6.4 Standardized 2D codes — classify the payload, don't special-case the brand

Today `usableQRURL` is a URL blocklist: http(s) links pass and get chased,
everything else is silently dropped. That is wrong in both directions once
standardized codes are in play — some non-URL payloads carry *identity*, and
some URL payloads must **not** be chased.

The right primitive is a **payload classifier**: one pure function, no new
dependency, that types each decoded code and routes it.

| Payload type | Example | Action |
|---|---|---|
| **GS1 Digital Link** | `https://id.gs1.org/01/09520123456788/21/1234` | parse AIs → **GTIN** (identity) + serial; optionally also chase the URL (it is designed to resolve to product info) |
| **GS1 element string** (DataMatrix, industrial/medical) | `(01)09520123456788(21)ABC123` | same parser → GTIN + serial |
| **Amazon Transparency — GTIN form** | SGTIN: AI 01 + AI 21 | **same parser** → GTIN + unit serial. Real identity signal |
| **Amazon Transparency — alphanumeric form** | `AZ…`/`ZA…` + 26 chars | opaque unit auth token → record, **never chase** |
| **Matter setup code** (smart home) | `MT:Y.K9042C00KA0648G00` | vendor ID + product ID → identity for CSA-certified devices |
| **Support URL** | `https://acme.com/support/wt41` | chase (current behavior) |
| **Platform page** | maker's YouTube channel | provenance only (already implemented) |
| **Non-web** | `WIFI:`, `mailto:`, `tel:` | drop (current behavior) |

**The key design point:** do not build "Amazon Transparency support". Build a
**GS1 Application Identifier parser** — and Transparency's GTIN form, GS1
Digital Link, and GS1 DataMatrix all fall out of the same ~50 lines. That
parser is also what makes the pipeline ready for **Sunrise 2027**, the GS1
migration of retail POS from 1D barcodes to 2D Digital Link QR codes, which
will put a GTIN-bearing QR on a large share of retail packaging.

Notes that change the implementation:

- **Transparency codes are DataMatrix, not QR.** gozxing ships a
  `datamatrix` reader (already vendored) — enabling it is a few lines in the
  decode pass, and it also picks up the industrial/medical DataMatrix labels
  that carry GS1 element strings.
- **Unit-unique serials populate a real field.** A Transparency/SGTIN serial
  is per-unit — that maps directly onto Homebox's `serialNumber`, which the
  sticker OCR often misses or misreads. Concrete accuracy win, no LLM.
- **Never send an auth token to a third party.** A unit-unique code is a
  tracking identifier; chasing it as a URL or putting it in a search query
  would leak it to a search engine for zero benefit. Classify-and-hold is both
  the correct and the privacy-preserving behavior.
- **EU Digital Product Passport** (ESPR, phasing in from 2027 for batteries and
  textiles) is the forward-looking case worth designing for but not building
  yet: a mandated per-product QR resolving to repair, spare-parts and
  documentation data — i.e. exactly docfetch's target, handed over by
  regulation. The classifier is the seam where it will plug in.

Value ordering: GS1 AI parser (covers Digital Link + DataMatrix + Transparency
GTIN) ≫ Matter codes ≫ Transparency alphanumeric (record only) — and the
classifier itself pays for its keep immediately by stopping the pipeline from
chasing payloads it should not.

### 6.5 Brand-protection code vendors (Scantrust, Securikett, NanoMatrix, …)

Surveyed because they issue a large share of the serialized codes appearing on
consumer goods. **Conclusion: integrate none of them individually.** They share
Amazon Transparency's shape and therefore its verdict:

- unit-level serialization for **anti-counterfeiting**, which is not docfetch's
  problem;
- a **covert security layer** (NanoMatrix's overt/covert `TrackMatriX Lock`
  layers, Scantrust's copy-detection patterns) that requires the vendor's own
  SDK *and* a high-resolution scan of the physical code — unusable and
  pointless for a downstream owner cataloguing an item they already possess;
- a **consumer content layer** behind a vendor-hosted resolver, whose APIs are
  brand-owner-gated exactly like Transparency's.

**The good news: the useful part already works.** These codes almost always
encode a *URL* to the vendor's resolver, which redirects to brand content. That
is our existing support-URL path — `resolveQR` already follows redirects and
`pdfLinksFrom` already harvests the destination's PDFs. No new code needed for
the common case.

**The one real gap is a latent bug.** `seedBrandCache` records
`rootDomain(finalURL)` as the manufacturer's domain. If a redirect terminates
on the *vendor's* host, docfetch would cache `Acme → scantrust.io` — the same
brand-cache poisoning already fixed for YouTube (§6.4 platform handling). These
hosts need the same treatment: **chase and follow, never seed the brand cache
from them**, and only accept a post-redirect domain as the brand domain when it
is not a known intermediary.

**The strategic point — and it validates §7.** This vendor space is converging
on GS1: Scantrust is a GS1-certified partner whose product generates **GS1
Digital Link** QR codes, and NanoMatrix builds its covert layers on top of a
GS1 Digital Link URL. So **integrate the standard, not the vendors** — the GS1
Application Identifier parser (A8) and the Digital Link resolver (C5) pick up
Scantrust-, NanoMatrix- and Transparency-issued codes as a side effect, with no
vendor-specific code and no commercial relationship. That is the whole argument
for §7's sequencing in one example.

*(Swapt: no reliable information found in review — it appears to sit in the
same "connected packaging" category, but nothing here should be treated as
verified until we see an actual code in the wild. The classifier's default
branch handles unknown payloads safely regardless, which is the point of
building a classifier rather than a vendor list.)*

### 6.6 Sequencing for this section

| # | Change | Cost |
|---|---|---|
| **A7** | Barcode + DataMatrix decode alongside QR (all photos, existing lib) + `intake.observed` event — **prerequisite for §7's resolver fast path** | ~0 |
| **A8** | QR payload classifier + GS1 AI parser (Digital Link / element string / Transparency SGTIN → GTIN + serial); stop chasing non-support payloads | ~0, ~50 lines |
| **A8b** | Intermediary-host guard: never seed the brand cache from brand-protection resolver domains (latent poisoning bug, same class as the YouTube fix) | ~0 |
| **A9** | FCC ID + origin country as vision schema fields (rides A6's evidence work) | prompt only |
| **B3** | Extra-photo UI + filesystem staging with TTL (kills double upload, feeds B0's corpus) | ~½ session |
| **C2** | GTIN/FCC/Matter → **resolver** lookups — see §7, which supersedes this row | new source + interface work |
| **C3** | Matter setup-code parsing (vendor/product ID) + certification marks as inventory metadata | prompt + fields |
| **C4** | EU Digital Product Passport resolution — design the seam now, build when codes appear | future |

## 7. Deterministic resolvers — asking instead of searching

This is the highest-ceiling idea in this document. Every identifier §6 extracts
(GTIN, FCC ID, Matter VID/PID) is a **key into a public database that already
knows where the manual is**. Where a resolver answers, the entire
search-and-guess pipeline is unnecessary.

### 7.1 The inversion

```
today          identity → search → rules+rerank → download → skim → gate → attach
with resolver  identity → resolver → authoritative doc URL → attach
```

Everything the middle of the pipeline exists to do — rank noisy candidates,
detect re-host spam, confirm the doc is the right product — is *definitionally*
satisfied when the manufacturer's own resolver hands back the link for that
exact GTIN. No rerank call, no skim download, no review gate. **Cheaper and
more accurate at the same time**, which nothing else in this document is.

That makes resolvers a **new trust tier above official-first** (D24), and a new
first stage in the discovery ladder:

```
resolve → qr → brand-site → web-pdf → web-html
```

### 7.2 What actually exists

| Resolver | Key | Returns | Availability |
|---|---|---|---|
| **GS1 Digital Link resolver** | GTIN | A **linkset**: request `Accept: application/linkset+json` or `?linkType=linkset` and get every link the brand published for that GTIN, each typed by the GS1 link-type vocabulary (`gs1:pip` product information page, `gs1:instructions`, `gs1:safetyInfo`, `gs1:certificationInfo`, …). Ask for the instructions link type and you are asking the brand for the manual | Public HTTP, no key. Coverage is the catch — it depends on the brand running/registering a resolver. **Sunrise 2027 is the inflection point** |
| **CSA Matter Distributed Compliance Ledger** | Matter VID + PID | Certification status, commissioning instructions, **links to product manuals**, product info, firmware version. Public **REST** API | Live now, no key. Covers Matter-certified smart-home devices |
| **FCC Equipment Authorization (OET/EAS)** | FCC ID | Grant records plus filing **exhibits — which routinely include the user manual as a PDF hosted on fcc.gov** | Public. Covers anything with a radio: huge share of modern electronics |
| Verified by GS1 / national GS1 registries | GTIN | Brand, product description, image | Partly membership-gated; useful for *identity*, not docs |
| Open GTIN databases (UPCitemdb, Open Food Facts, …) | GTIN | Name, brand, sometimes images | Free, coverage patchy and consumer-goods skewed. Fallback only |
| **Amazon Transparency** | T-code | Authenticity + brand-configured content | ❌ **Not usable.** The customer-facing content layer is real, but it is gated behind Amazon's own scanning app, and the Transparency APIs are provisioning/verification endpoints for *enrolled brand owners* — there is no third-party lookup. Its value to docfetch is only the **GTIN + serial embedded in the SGTIN form** (§6.4) |

### 7.3 Why this changes the priority of §6

Identifier extraction (A7/A8) looked like an identity-accuracy improvement. It
is actually **the key to the fast path**: a decoded GTIN is not just a better
search term, it is a resolver query that can return the manufacturer's own
manual link with no search at all. That raises A7/A8 from "nice, free" to
"prerequisite for the best thing in the roadmap".

### 7.4 Architecture: resolvers are not search providers

`SearchProvider` (plan M3, §4.1) returns *candidates requiring verification*.
A resolver returns an *authoritative answer*. Different contract, different
trust tier, different position in the ladder — so it is a **separate
interface**, not another SearchProvider implementation:

```go
type ResolverProvider interface {
    // Resolve returns typed documents for an identifier, or nil when the
    // resolver has no record. A hit is treated as official provenance.
    Resolve(ctx context.Context, id Identifier) ([]ResolvedDoc, error)
}
```

with `gs1`, `matter-dcl`, `fcc` implementations. This is precisely the
"provider standardization" backlog item, and resolvers — not a second search
engine — are the trigger that finally justifies doing it.

### 7.5 Honest limits

- **Coverage is thin today.** DCL covers Matter devices only; FCC covers radio
  devices only; GS1 Digital Link coverage depends on brand adoption and is
  early (Sunrise 2027 is the bet, not the present). Resolvers are a **fast
  path, not a replacement** — search stays as the fallback for everything else,
  which is most of a homelab inventory today (the water timer resolves nowhere).
- **A resolver hit still needs the content-class check.** "The brand's link for
  this GTIN" can still be a spec sheet rather than a manual; keep the class
  gate, drop only the product-identity checks.
- **Do not send unit-unique serials to resolvers** — query the GTIN (AI 01),
  never the serial (AI 21). Same privacy rule as §6.4.

### 7.6 Sequencing

| # | Change | Cost |
|---|---|---|
| **B4** | `ResolverProvider` interface + `resolve` as ladder stage 0 (provider standardization lands here) | ~1 session |
| **B5** | FCC ID resolver — highest present-day coverage for electronics, and filings carry actual manuals | ~½ session |
| **B6** | Matter DCL resolver — small, public REST, exact manual links for smart-home devices | ~½ session |
| **C5** | GS1 Digital Link resolver + linkset parsing — low coverage now, the strategic bet on Sunrise 2027 | ~½ session |
| — | Open GTIN databases as an identity fallback only | opportunistic |

## 8. Accuracy backlog (beyond the above)

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
