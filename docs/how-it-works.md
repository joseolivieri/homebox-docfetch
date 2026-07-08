# How docfetch works

*A plain-language tour of the pipeline. For the technical spec see
[spec.md](spec.md); for why things are built this way see
[decisions.md](decisions.md).*

docfetch is a small helper service that lives next to Homebox (the home
inventory app). Its job: **every item in the inventory should end up with its
user manual, complete metadata, a product photo, and warranty info — without
anyone doing that work by hand.** It talks to Homebox only through the same
API the web app uses; it never touches the database directly.

It has two stages, with a deliberate division of labor:

- **Intake** (the portal) — a phone-friendly web page (private, Tailscale-only)
  for adding a new item by taking photos of it. Its only outside call is the
  AI vision model that reads your photos — it never searches or downloads
  anything from the web. That keeps it simple, fast, and ready to run fully
  offline once a local vision model replaces the cloud one.
- **Curation** (the scanner) — a background job that wakes up regularly, looks
  at every item, and fills in whatever is missing. *All* web activity lives
  here: metadata lookups, manual hunting, official product photos, warranty
  research.

The two never talk to each other directly — the inventory itself is the
hand-off. Intake creates the item; the scanner notices the change within about
30 seconds and takes over.

---

## 1. Adding an item with your phone

Open the portal, and you get a four-photo grid:

| Photo | What it's for |
|---|---|
| **Model sticker** | The label with the model/serial number — the main identity source |
| **Receipt** | Fills in where/when it was bought and the price |
| **Product** | Your own photo of the item; becomes its main image |
| **Warranty** | Warranty card/leaflet, if there is one |

Take whichever ones you have (any subset works), and an AI vision model reads
them: manufacturer, model number, serial number, purchase details, warranty
duration. You get a confirmation screen where you can correct anything, pick a
quantity, and optionally choose which room the item lives in. Tap create, done
— usually under a minute in total.

Behind the scenes the portal then creates the item in Homebox with everything
you confirmed and attaches all the photos you took (receipt as a receipt,
warranty as a warranty, your product photo as the main image). That's it —
intake is done. Everything that needs the web happens in the curation stage,
starting seconds later:

- **Official product photo.** The scanner searches the web for one, has the
  vision model compare candidates against *your* photo of the item (fetched
  back from the inventory as the reference), and attaches the best one — but
  only if it's confident enough. A bad photo is worse than none.
- **Warranty estimate.** It searches for the manufacturer's standard warranty
  terms. Only when it finds a solid, sourced answer does it set a hard expiry
  date; a shaky answer becomes a note instead of a date.
- **Metadata and the manual** — the next two sections.

## 2. Filling in metadata (enrichment)

Items often start life as just a name ("Anker 737 Power Bank"). The scanner
tries to complete the identity: **manufacturer, model number, full product
name, category.**

The rules here are strict, because wrong metadata poisons everything
downstream:

- **Fill-only.** It writes into empty fields only. It never overwrites
  anything a human typed.
- **Corroboration, not confidence.** An AI saying "I'm 95% sure" is not
  evidence. A value is written only when it appears on at least two
  independent websites *and* a reverse search ("Anker A1289") leads back to
  the original product.
- **Real part numbers only.** The marketing number in a product's name ("737")
  is not the model number; the actual SKU (A1289) is what makes manual
  searches precise.

A correct model number is the single biggest accuracy win: it turns a vague
search into an exact one.

## 3. Finding the manual

The scanner works through sources in order of trustworthiness, stopping as
soon as one produces a confident match:

1. **The manufacturer's own site.** First figure out the official domain
   (anker.com, lg.com), then search only there, open the support pages, and
   collect the PDF links they point to. A manual from the maker's own support
   page is the gold standard.
2. **General web search for PDFs.** Cast wider, but with guards: known
   junk sources are blocked (e.g. marketplace image CDNs, where "manuals" are
   seller uploads for possibly different products), English/US sources are
   preferred, and file signatures are checked.
3. **Manual web pages.** Some manuals only exist as web pages, not PDFs.
   Those can't be attached as files, but they can be linked.

Candidates are scored by rules first (model number in the URL/title, official
domain, PDF vs page). Only when rules can't decide does a small, cheap AI
model break the tie — it sees only titles and snippets, never whole documents,
which keeps costs near zero.

**Verification before attaching.** A downloaded file must actually be a PDF,
and its first pages are skimmed by the AI to confirm it's about *this*
product — this is what stops a Soundcore manual landing on an Anker charger.
One exception: files harvested directly from the manufacturer's own support
page skip the content check, because where it came from is stronger evidence
than a skim.

## 4. What lands on the item

Depending on what was found:

- **A confident PDF** → attached to the item as its manual (stored locally in
  Homebox, so it survives the source link dying), plus two custom fields:
  **Manual** (the PDF's source, as a `[pdf](…)` link) and **Manual (web)**
  (the official support page, as `[web](…)`).
- **Only a web-page manual** → no file; the **Manual (web)** field links it.
- **A maybe** → nothing is attached. You get a phone notification instead
  (see below).
- **Nothing** → the item is retried later, with increasing gaps between
  attempts so dead ends don't burn resources.

Every action is logged inside the item's notes, in a fenced block that never
touches your own notes text — a dated, one-line-per-event audit trail:

```
#### docfetch
- 2026-07-08 meta manufacturer=Anker (1.00), modelNumber=A1289 (1.00) [src](anker.com)
- 2026-07-08 photos: sticker, receipt, product
- 2026-07-08 photo (0.95, matched) [src](…)
- 2026-07-08 manual attached (0.90) [pdf](…)
```

The numbers in parentheses are confidence scores. URLs always appear as short
labeled links, never raw. (The extra detail lines are the opt-in
`notes.audit_log` setting.)

## 5. When it's not sure: the review notification

Below the confidence bar, docfetch never guesses silently. It tags the item
`docfetch/unverified` and sends a phone notification (ntfy) with two buttons:

- **Attach** — your approval is noted on the item, and the scanner downloads
  and attaches that exact document within about 30 seconds (skipping its usual
  content checks — a human said yes).
- **Reject** — the candidate is marked wrong, permanently.

Both buttons are one tap and work the same way: they write a small `approved`
or `rejected` line into the item's notes, which the scanner acts on. The links
are cryptographically signed, so nobody without the key can trigger them.

## 6. How it learns (the feedback loop)

Every decision the pipeline makes — which sources it considered, what it
scored them, what it chose, how confident it was — is recorded in a local
ledger. Over time, real-world outcomes label those decisions:

- **Rejected** — you tapped Reject, or deleted an attached manual. That URL is
  never proposed again for that item; the next scan re-searches without it.
  (You can also reject by hand: add a `- rejected <url>` line to the item's
  docfetch notes block.)
- **Overridden** — you changed a metadata value the machine wrote. The machine
  treats your edit as final and will never re-fill that field.
- **Confirmed** — an attached manual survived a month untouched. Silence is
  approval.

A weekly digest notification summarizes the backlog and the week's outcomes.

The ledger is the raw material for the next improvements: a regression test
set built from confirmed answers (so pipeline changes can be tested against
known-good results before deploying), automatically learned good/bad source
domains, and data-driven tuning of the confidence thresholds.

## 7. Guardrails

- **Human data is sacred.** Fill-only writes; a human correction is never
  overwritten or re-filled; machine writes are always distinguishable (notes
  log + audit tables).
- **No junk.** Better to attach nothing than the wrong manual — hence
  verification, blocklists, and the review gate.
- **Cheap by design.** Rules do most of the work; the AI is a tiebreaker that
  reads snippets, not documents. Search runs through a self-hosted engine.
  Whole thing idles at ~zero cost.
- **Private by design.** The portal and its approve/reject endpoints are only
  reachable inside the private network (Tailscale); action links are signed.

## 8. The knobs

Everything above is tunable in one YAML file (see
[config.example.yaml](../config.example.yaml)): scan schedules, source order,
confidence thresholds, language preference, the audit log, notification
targets. Deployment (and the file itself) is managed by Ansible.
