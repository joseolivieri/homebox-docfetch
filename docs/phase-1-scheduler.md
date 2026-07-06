# Phase 1 — Scheduler (doc-fetch sidecar)

Deliverable: a deployed Go service that scans the Homebox collection on a configurable schedule and
auto-attaches user manuals to items, with an ntfy review-gate and a weekly reconcile digest.

**Prereqs:** resolve Q1 (token) and Q3 (gate) in [decisions.md](decisions.md) before P1-02 / P1-06.
Read [spec.md](spec.md) §6 (API) before any client work.

Task IDs are stable and referenceable. Each task lists: intent · files · acceptance · deps.
Status: `[ ]` todo · `[~]` in progress · `[x]` done.

---

- [ ] **P1-00 · Scaffold module & repo skeleton**
  - Intent: Go module, dir layout per README, `config.example.yaml`, empty package stubs, Makefile/build.
  - Files: `go.mod`, `cmd/docfetch/main.go`, `internal/{config,homebox,store,discovery,llm,notify,scheduler}/`, `config.example.yaml`.
  - Acceptance: `go build ./...` succeeds; `docfetch --help` lists `scheduler|once|portal` subcommands.
  - Deps: none.

- [ ] **P1-01 · Config loader**
  - Intent: load YAML property file, env-interpolate `${VAR}`, validate, typed struct matching spec §5.
  - Files: `internal/config/`.
  - Acceptance: loads `config.example.yaml`; missing required/invalid cron → clear error; secrets resolved from env.
  - Deps: P1-00.

- [ ] **P1-02 · Homebox API client** *(blocked by Q1)*
  - Intent: typed client for the entity model — `ListEntities(page,tags)`, `GetEntity(id)`, `CreateEntity`,
    `PatchEntity`, `UploadAttachment(id,file,name,type,primary)`, `ListTags`/`CreateTag`, `ListEntityTypes`.
    Bearer auth. Create+Patch included now for Phase-2 reuse even though scheduler only lists+attaches.
  - Files: `internal/homebox/`.
  - Acceptance: against the live instance — list paginates; upload a test PDF as `type=manual` and see it on the
    entity; tag create/list idempotent. Attachment dedupe reads `EntityOut.attachments`.
  - Deps: P1-01, Q1.

- [ ] **P1-03 · Startup bootstrap**
  - Intent: resolve `item_entity_type` name → id; ensure `unverified_tag`/`provenance_tag` exist (create if missing) → ids.
  - Files: `internal/homebox/` (bootstrap), wired from `scheduler`.
  - Acceptance: on first run creates missing tags; on subsequent runs is a no-op; ids cached for the process.
  - Deps: P1-02.

- [ ] **P1-04 · SQLite state store**
  - Intent: persist per-entity record `{entity_id, name, meta_hash, updatedAt, status, doc_sha256, doc_url,
    attempts, first_seen, last_checked, last_attached}`. Drives idempotency, backoff, change detection.
  - Files: `internal/store/`.
  - Acceptance: schema migrates on boot; upsert + query by status; survives restart at `state_db` path.
  - Deps: P1-01.

- [ ] **P1-05 · Discovery pipeline** *(LLM step blocked by Q2)*
  - Intent: build queries from `{manufacturer, modelNumber, name}` → SearXNG → candidates
    `{title,url,snippet}` → rules filter/score (model# match, `application/pdf` via HEAD, size sanity) →
    if ambiguous, OpenRouter rerank (`internal/llm`, JSON out `{best,confidence}`, snippet≤150) → confidence score.
  - Files: `internal/discovery/`, `internal/llm/`.
  - Acceptance: given a real `{mfr,model}` returns ranked candidates + confidence; rules-only path never calls LLM;
    respects `rate_limit_per_min`; negative results cached with backoff.
  - Deps: P1-01, P1-04, Q2 (for the rerank step; rules path testable without).

- [ ] **P1-06 · Attach + confidence gate** *(gate policy per Q3)*
  - Intent: download best candidate → SHA-256 → dedupe vs entity attachments → if `>= auto_attach_threshold`
    (and model-match if required) upload `type=manual`; else review-gate (ntfy notify w/ candidate link, mark pending).
  - Files: `internal/discovery/` (attach), `internal/notify/`, wire in `scheduler`.
  - Acceptance: high-confidence attaches once and is idempotent on re-run; low-confidence sends one ntfy and does
    not attach; identical doc never re-attached; `skip_if_manual_exists` honored.
  - Deps: P1-02, P1-04, P1-05, Q3.

- [ ] **P1-07 · Scheduler jobs (in-process cron)**
  - Intent: cron per spec — `scan_new` (new items → discovery initial), `followup` (stale > `followup_after` →
    discovery followup), `reconcile` (`?tags=unverified` → weekly ntfy digest count). `once` subcommand runs one scan.
  - Files: `internal/scheduler/`, `cmd/docfetch/`.
  - Acceptance: cron entries registered from config; `docfetch once` runs a full scan and exits 0; overlapping runs guarded.
  - Deps: P1-03, P1-04, P1-06.

- [ ] **P1-08 · Dockerize**
  - Intent: multi-stage build → minimal final image; non-root; `/data` volume for `state_db`; `docker-compose.yml`
    with `docfetch` + `searxng` services on a shared network.
  - Files: `Dockerfile`, `docker-compose.yml`.
  - Acceptance: `docker compose up` starts both; docfetch reaches searxng by service name; state persists across restart.
  - Deps: P1-07.

- [ ] **P1-09 · Ansible role + deploy** *(SearXNG + docfetch)*
  - Intent: `ansible/roles/docfetch/` mirroring the `homebox` role — deploy dir, `.env` from vault, `docker_compose_v2`,
    handler restart. Add `HOMEBOX_TOKEN`, `OPENROUTER_API_KEY` to `ansible/secrets.yml`. Tailscale-only exposure.
  - Files: `ansible/roles/docfetch/{tasks,defaults,handlers,templates}/`, `ansible/secrets.yml`, playbook wiring.
  - Acceptance: `cd ansible && ansible-playbook ... --limit compute-1` deploys cleanly; service healthy; scan runs on schedule.
  - Deps: P1-08, Q1, Q2.

- [ ] **P1-10 · Docs + memory**
  - Intent: update `docs/architecture.md` (service inventory, compute-1 table, access tier, Ansible roles table);
    add a `project_docfetch` memory; note SearXNG as a new service.
  - Files: `docs/architecture.md`, memory dir.
  - Acceptance: architecture doc reflects docfetch + searxng; memory index updated. (Same commit as P1-09 per CLAUDE.md.)
  - Deps: P1-09.

## Phase 1 exit criteria

New Homebox items get manuals auto-attached within a scan cycle; follow-ups catch newly-published docs without
re-attaching duplicates; low-confidence finds arrive as ntfy prompts; a weekly digest lists items awaiting review;
the whole thing runs from a vault-managed Ansible deploy on compute-1, Tailscale-only.
