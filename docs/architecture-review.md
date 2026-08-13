# Architecture review — systems, product/logistics, and a second pass

Reviews `docs/pipeline-accuracy.md` (the accuracy plan) and the shipped code
behind it, from two lenses, then re-reviews both. Severity: **H** blocks a
public release, **M** should be fixed before broad use, **L** worth knowing.

---

# Part 1 — Systems architecture review

Overall: the stack is **well-chosen and deliberately boring** — Go static
binary, modernc SQLite, in-process cron, pure-Go decoding, no queue, no vector
DB. That is the right shape for a homelab service and most of it should not
change. The findings below are about the gap between "works on compute-1" and
"a stranger runs this from a README".

### S1 · SQLite is configured for the old architecture — **H**

`internal/store/store.go:57-61` opens the DB with no pragmas and
`SetMaxOpenConns(1)`. That was correct when the scanner was alone in its
process. It no longer is: since D25 the portal shares it, and since M2 the
portal polls `/api/events` **every 3 seconds** while `/log` pages issue their
own queries — all serialized behind a single connection that a long scan pass
can hold.

Fix is standard and small: `_journal_mode=WAL`, `_busy_timeout=5000`,
`_synchronous=NORMAL`, keep one *writer* connection but allow concurrent
readers. Without it the first symptom under load is the portal UI hanging
mid-scan, which reads as "the app is broken".

### S2 · The extraction recommendations contradict a locked decision — **H (as designed)**

`pipeline-accuracy.md` B2/C1 propose shelling out to `pdftotext` and
`tesseract`. The plan flags the image-size cost but **not the real conflict**:
D25 collapsed deployment to *one* container and the project's stated convention
is a CGO-free static binary on distroless. Adding two external binaries
reverses both.

Three honest options, in preference order:

1. **Optional extractor sidecar** — a `docfetch-extract` container (poppler +
   tesseract behind a tiny HTTP endpoint), absent by default; core degrades to
   the built-in reader when it is not configured. Keeps the main image static
   and makes OCR opt-in *by not deploying it*.
2. **Two image tags** — `:latest` (static, current) and `:full` (poppler +
   tesseract). Simple, familiar to the self-hosting audience, doubles CI.
3. Fatten the single image — simplest, but taxes every user for a feature most
   will not need, and abandons distroless-static.

The sidecar contradicts D25's "one blessed shape" and that tension must be
decided explicitly, not discovered during implementation. **Recommendation: (1)
with (2) as fallback** — and either way it needs a new D-row.

### S3 · No spend guard — **H for public release**

Nothing in the codebase caps LLM usage (`grep budget|maxCalls|spend` → nothing).
The author's deployment survives on a curated ~$5/mo budget and a 21-item
collection. A stranger pointing this at a 2,000-item inventory triggers a first
scan that enriches, reranks, skims and vision-ranks across the whole
collection on their key. That is the worst possible first-run experience and
the fastest route to a bad reputation.

Needs: `llm.max_calls_per_day` / `max_spend_per_day` with a hard stop and a
loud log line, plus a documented "first run will process N items" warning and
ideally a `--dry-run` that reports the projected call count.

### S4 · Portal has no authentication and now serves the whole activity log — **H for public release**

The portal was designed for tailnet-only exposure with the tailnet *as* the
auth boundary (`server.go:4`). Since M2 it also serves `/log` — the full
inventory activity across every item — and the HMAC-signed action endpoints.
For a public project, someone will port-forward it.

Minimum: default `listen` to `127.0.0.1:8099` in the shipped config (note:
`config.dev.yaml` deliberately binds `:8099` for phone testing — that default
must not leak into the production example), support a trusted-proxy auth header
or basic auth, and document the reverse-proxy pattern prominently. A README
line saying "put it behind Tailscale" is not sufficient once `/log` exists.

### S5 · HMAC key is the Homebox API token — **M**

`internal/sign` signs action links with `cfg.Homebox.Token`. Reusing a live API
credential as a MAC key is a smell: it widens the blast radius of any signing
oracle and couples two rotation lifecycles. Derive it (HKDF from the token with
a fixed context string) or take a separate `intake.sign_key`, defaulting to
derived so nothing breaks.

### S6 · No migration framework, and the plan adds three tables — **M**

Still `CREATE TABLE IF NOT EXISTS` (plan M3 has the fix). The accuracy plan now
adds `facts` (§6.6), staging metadata (§6.2) and resolver caching (§7). Shipping
schema changes to external users without numbered, forward-only migrations is
how you corrupt someone's data — and `facts` is explicitly *not* re-derivable
once staged photos TTL out (§6.6), so it is real state.

### S7 · Scan is fully sequential — **M**

`scheduler.Run` guards every job with one mutex and `Scan` walks items one at a
time; each item can spend 30s per download (`discovery.go:140`) plus LLM
latency. A 2,000-item first pass is measured in hours, during which the
change-poll is starved (it shares the mutex) so new intakes wait. A bounded
worker pool (`curation.concurrency`, default 2–4) with the existing per-entity
inflight guard is a contained change; the rate limiter already protects the
egress side.

### S8 · Dependency audit — **L, mostly good news**

Seven direct dependencies, all pure-Go: `modernc.org/sqlite`, `robfig/cron/v3`,
`gopkg.in/yaml.v3`, `golang.org/x/image`, `makiuchi-d/gozxing`,
`ledongthuc/pdf`. Boring and appropriate.

The single weak link is the one the plan already names: `ledongthuc/pdf`
(pinned to an untagged pseudo-version, panics on malformed input, silently
empty on CID fonts). Note the plan under-sells one alternative — `gozxing`
already earns its keep for QR and, per §6.4, DataMatrix/barcode, so the
dependency count does not grow for the highest-value identity work.

### S9 · Observability is absent — **M**

No `/healthz`, no `/readyz`, no metrics, `log.Printf` throughout. Already in
plan M3. For a service whose whole value is unattended background work,
"is it working?" currently has one answer: read the activity log. That is
actually decent — the event log is better instrumentation than most projects
have — but a container healthcheck and a scrape endpoint are table stakes.

### S10 · Config surface is a first-run barrier — **M**

`config.example.yaml` is ~150 lines. The plan's M4 "config minimization" is
right and should be treated as release-blocking, not polish: a 15-line
quickstart config with everything else defaulted, and the current file demoted
to a reference appendix.

### S11 · Prompt-injection surface, currently latent — **M, rising**

`Skim` feeds text extracted from **fetched, untrusted PDFs** into an LLM whose
verdict gates attachment. A crafted document can assert "this is the manual for
WT41". Impact today is bounded (it can win an attach it would otherwise lose,
which a human can delete) — but the plan's direction *raises* it: resolver
results, and especially the email ingest suggested in Part 2, turn this into a
path where attacker-controlled text influences what gets fetched and stored.
The mitigation is architectural and cheap if adopted early: treat all
model output about fetched content as *evidence, never instruction*, and never
let skim output introduce a new URL to fetch.

### S12 · Accuracy/performance/cost expectations are unstated — **M**

For a public project the README must answer: how often is it right, how long
does a first scan take, what will it cost per month. Right now none of the
three has a number — which is R5's measurement gap wearing a different hat. A
public release needs at minimum: "on a 20-item test set, X% auto-attached
correctly, Y% review-gated, Z% not found; first scan ≈ N minutes/100 items;
≈ $A/month at default settings."

---

# Part 2 — Product & logistics review

The question behind this lens: **docfetch is photo-first, but is a photo the
best available lead?** Often it is not. The item's *purchase* is already a
digital record, and its *identity* is often already on the network.

### P1 · Purchase-side data is the largest untapped lead class — **high value**

An order confirmation contains what photo intake struggles to produce: the
exact retailer SKU, the **product URL on the manufacturer's or retailer's
site**, the precise purchase date and price, and often the full product name.
A product URL is a *better* lead than a model number — it is the brand-site
stage's destination handed over directly, no search required.

Channels, easiest first:
- **Forward-an-email address / watched IMAP folder** — user forwards order
  confirmations; docfetch parses structured order data.
- **Retailer order-history exports** (Amazon offers CSV order reports) — bulk
  backfill of an entire purchase history.
- **PDF invoices** already attached at intake (partially covered today: the
  receipt photo path extracts purchase fields but not product URLs).

Caveats are severe and covered in Part 3 — this is the highest-value *and*
highest-risk item in this review.

### P2 · Barcode-only fast path — **high value, honest limits**

Once §6.4/§7 land, the fastest intake for anything boxed is: scan the barcode,
get GTIN, resolve, done — no sticker photo, no vision call, no search. For
retail-packaged goods this is a dramatically better UX than four photos.

But §7 admits resolver coverage is thin today, so this must be built as
"barcode → GTIN → *try* resolver → fall back to search", never marketed as
instant. It gets better automatically as Sunrise 2027 adoption grows.

### P3 · The item is often already on the network — **medium value, high friction**

For a homelab audience, a meaningful share of documented-worthy items are
network devices: mDNS/SSDP/Matter commissioning records expose make, model and
firmware without a photo at all. This is the most *audience-native* lead source
available and nobody else is doing it.

It also cuts against the project's clean egress story (Part 3) and needs
explicit consent. Probably an optional module, off by default — but worth a
design sketch because it is a genuine differentiator.

### P4 · "I already have the manual" — **low cost, immediate value**

There is no path to hand docfetch a PDF you already have. A drop-a-file action
would: satisfy the user instantly, record a confirmed URL/hash, and **seed the
golden set (B0/B1) with ground truth** — the measurement gap's cheapest input.
Smallest item in this review with the best value ratio.

### P5 · Text/voice description intake — **low cost**

"Whirlpool dishwasher WDF520PADM7, bought at Lowe's in 2019" is a complete
identity with no photo. The confirm screen already accepts manual entry, so
this is mostly a UI affordance plus optionally an LLM parse of one free-text
line. Serves the case where the item is not in front of you.

### P6 · Bulk intake — **medium**

Photographing items one at a time does not scale past a drawer. Batch mode
(N photos → N items, or one photo of several boxes) is a real workflow gap for
the "I want to catalogue my house" use case that Homebox users actually have.

### P7 · Serial → manufacturer registration lookup — **low-medium**

Many manufacturers expose product-registration or warranty-status lookup by
serial number. Where public, this confirms model *and* warranty dates from the
authoritative source. Fits the §7 resolver interface exactly, per brand.

### P8 · Where photo intake genuinely fails

Worth stating plainly, because it is exactly where documentation matters most:

| Case | Why photos fail |
|---|---|
| Installed appliances (HVAC, water heater, built-in oven) | label is behind/inside the unit; often unreachable |
| Old appliances | label worn, faded, or painted over — **and these are the items whose manuals are hardest to find**, so the pipeline is weakest precisely where it is needed most |
| Items long out of packaging | no box, no receipt, no QR |
| Small/no-label goods | tools, fixtures, furniture |
| Bulk cataloguing | throughput, not capability |

P1 (purchase records) and P5 (description) are the direct answers to rows 2–4;
P3 answers network devices; nothing answers the installed-appliance case except
the user typing a model number they read once with a flashlight — which argues
for making manual entry excellent rather than treating it as a fallback.

### P9 · Label-location hints — **low cost, real UX**

Where the model label lives is category-knowledge the pipeline could volunteer
("dishwashers: inside the door edge; water heaters: side panel near the
thermostat"). Trivial as static content keyed on the category we now extract
(§1.4's `productType`), and it directly improves the quality of the input photo
— which is the cheapest possible accuracy intervention.

---

# Part 3 — Second pass: reviewing the reviews

Re-reading Parts 1 and 2 critically, plus the plan they review.

### M1 · The plan has outgrown its implementation — **the most important finding**

`pipeline-accuracy.md` is now ~700 lines describing three tiers, nine resolver
and intake workstreams, and a storage design. **Zero tier-A items have
shipped.** Every session has added scope; none has removed any. Parts 1 and 2
above just added ~20 more findings.

This is the failure mode the plan itself diagnoses (R5: unmeasured work) turned
one level up: we are planning faster than we are learning. Concrete
recommendation: **freeze the plan document**, ship all of tier A plus B0
(bench-vision), then re-plan from measured results. Nothing in Parts 1–2 except
S1 and S3 should jump that queue.

### M2 · Part 1's sidecar recommendation quietly reverses D25

S2 offers a sidecar as the preferred option, but D25 was decided *specifically*
to collapse two containers into one and to have exactly one blessed deployment
shape. Recommending a second container is not a neutral technical choice — it
partially undoes a locked decision made three days ago.

Resolution: the sidecar is acceptable **only** as an optional component that
is absent in the default compose file, with the core fully functional without
it. If that is not achievable, take option (2) (two image tags) instead, which
preserves "one container" per deployment. Either way it needs a D-row that
explicitly amends D25 rather than silently contradicting it.

### M3 · Part 2's email ingest is a project-swallowing risk — **downgrade it**

P1 is correctly identified as the highest-value lead source, but Part 2
under-weights three compounding costs:

1. **It breaks the two-stage model.** Email is a *third* input surface with its
   own credentials, polling, and failure modes — the intake/curation boundary
   (D20) has no place for it.
2. **It is a prompt-injection vector into a fetching agent** (S11). Untrusted
   email text → LLM → URLs that docfetch then fetches and attaches. Anyone can
   send the user an email. This is the most dangerous idea in either review.
3. **Credential handling.** IMAP passwords or OAuth for a service whose current
   security posture is "no auth, run it on a tailnet".

It should not be dropped — the value is real — but it must be reshaped:
**parse only user-initiated uploads** (drag a `.eml` or a PDF invoice in,
exactly like P4's file drop) rather than watching a mailbox. Same data, no
credentials, no polling, no unsolicited attacker input. If mailbox watching
ever happens it belongs in a separate opt-in module with its own threat model.

*(Resolved: `pipeline-accuracy.md` §6.7 adopts exactly this shape, folding P4,
P5 and this reshaped ingest into a single typed-document intake feature.)*

### M4 · Part 2 over-promises the barcode fast path

P2 says "scan barcode, done". §7 says resolver coverage is thin today. Both are
in this repo and they disagree in tone. The honest framing for users:
"barcodes make identification exact; whether we can *resolve* a manual from one
depends on the brand — today, often not." Ship it with search fallback and no
marketing claim.

### M5 · Part 2's network discovery contradicts the project's core promise

P3 is genuinely differentiating, but docfetch's architectural selling point is
a *clean, auditable egress boundary* — intake touches only the vision model.
Adding LAN scanning introduces a capability that is neither intake nor curation
and that some users will consider invasive. Verdict: keep the design sketch,
but this is a **post-1.0, opt-in, separate-module** feature at best. Do not let
it into the core roadmap now.

### M6 · Neither review cut anything — so here are the cuts

A review that only adds is not a review. From the existing plan:

- **C3 (certification marks)** — §6.3 already rates them lowest value and §6.6
  requires every fact to have a consumer. Nothing consumes CE/UKCA. **Cut until
  an inventory feature needs it.**
- **C1 (OCR)** — defer harder than the plan states. It is the single most
  expensive item (image size, latency, second external binary) and B2 +
  A3's provenance policy may reduce the unverified-attach problem enough that
  it is never worth it. Gate it explicitly on A2's measured numbers.
- **C4 (Digital Product Passport)** — correctly parked, but it should be a
  *watch item* with no design work until a real DPP code exists to test against.
- **A9 (origin country)** — rated "cheap, take it", but no stage consumes it
  either. Same rule as C3: skip until the region logic actually reads it.

### M7 · The measurement gap is now worse, not better

Every session since R5 was written has added surface without adding a metric.
There are now ~30 proposed changes and still no number that any of them would
move. B0 (`bench-vision`) is the cheapest possible first measurement and it is
still unbuilt. If only one thing ships from this entire review, it should be
B0 — and if two, add S1 (WAL) because it is a latent production bug in code
that already shipped.

### M8 · What Part 1 got right that should not get lost

The stack findings are mostly "make the shipped thing safe for strangers" (S1,
S3, S4, S6, S10, S12) rather than redesign. That is the correct conclusion and
worth stating positively: **the architecture is sound; the gaps are release
engineering.** No finding in this review argues for changing the language, the
database, the deployment model, or the LLM strategy.

---

## Consolidated priority after all three passes

| Rank | Item | Why |
|---|---|---|
| 1 | **S1** WAL + busy_timeout | latent production bug in shipped code |
| 2 | **B0** bench-vision harness | closes the measurement gap at lowest cost; fixes the root input |
| 3 | **Tier A** (A1–A8b, minus A9) | free accuracy work already specified |
| 4 | **S3, S4** spend guard + portal auth defaults | public-release blockers |
| 5 | **S6, S10, S12** migrations, config minimization, published expectations | public-release blockers |
| 6 | **§6.7 typed document intake** (A10/A11 first) | absorbs P4 + P5 + reshaped M3 into one feature; a dropped manual short-circuits the whole pipeline and seeds the golden set |
| 7 | **B1/B2** replay harness, then extraction | measured, in that order |
| 8 | **§7 resolvers** (B4–B6) | highest ceiling, after identifiers land |
| 9 | Everything else | re-plan after 1–8 |
