# homebox-docfetch

Standalone sidecar that enriches a [Homebox](https://homebox.software) inventory
through its REST API — **no UI integration, no DB access**. Two discrete stages (D20):

- **Intake** (`internal/portal`) — phone photo-intake PWA. Vision-model
  calls only; no web searching (offline-LLM-ready).
- **Curation** (`internal/scheduler`) — recurring scanner owning ALL
  web egress: metadata enrichment, manual fetching, official photos, warranty, tagging.

Both stages run in one process (`docfetch serve`, D25); the egress boundary is enforced at
the package level (`internal/portal` imports no discovery/egress code). A shared sqlite
event store is the bus between them (D26): signals (qr/approve/reject) and the full audit
trail are events, browsable at the portal's `/log` pages or via `docfetch log`; entity
notes carry a one-line breadcrumb. See
[`docs/plan-architecture-v2.md`](docs/plan-architecture-v2.md).

This repo holds the service source, its container/compose, and its design docs. It is
deployment-agnostic: run it anywhere `docker compose` works, configured via `config.yaml`
(see `config.example.yaml`) plus secrets from env. CI publishes a linux/amd64 + linux/arm64
image to `ghcr.io/joseolivieri/homebox-docfetch` (`latest` on main, semver on `v*` tags). One
reference deployment exists as the `docfetch` Ansible role in the author's private homelab repo.

**New here? Start with [`docs/how-it-works.md`](docs/how-it-works.md)** — a plain-language
walkthrough of the whole pipeline (intake, enrichment, manual discovery, review gate, learning loop).

## Dev quickstart

```bash
make env       # creates .env — uncomment + fill DOCFETCH_HOMEBOX_URL, HOMEBOX_TOKEN, OPENROUTER_API_KEY
make dev       # searxng in docker (loopback :8080) + native `go run serve` on config.dev.yaml
```

Portal + activity log: http://localhost:8099 (log at `/log`). Other targets:
`make once` (single scan pass), `make log` (events in the terminal), `make probe`
(Homebox client smoke test), `make dev-docker` (full stack containerized),
`make reset` (wipe dev state — collections are disposable in dev, D26),
`make check` (build + tests). Dev state lives in `./data/` (gitignored).

Point it at a Homebox instance whose collection you can afford to reset — the
pipeline tags, enriches, and attaches to whatever it sees.

## Agent read order

Read these before touching code. They are the source of truth; do not re-derive facts from
classic-Homebox knowledge — this deployment runs the **entity-model** fork (see spec §API).

1. [`docs/spec.md`](docs/spec.md) — goals, architecture, config schema, **authoritative Homebox API reference**.
2. [`docs/decisions.md`](docs/decisions.md) — locked decisions + **open questions that block work**.
3. [`docs/phase-1-scheduler.md`](docs/phase-1-scheduler.md) — Phase 1 task board (build this first).
4. [`docs/phase-2-portal.md`](docs/phase-2-portal.md) — Phase 2 task board (additive; do not start until Phase 1 lands).

## Planned layout

```
homebox-docfetch/
  cmd/docfetch/         # single binary; subcommands: serve | once | probe (scheduler/portal deprecated)
  internal/
    config/             # property-file (YAML) loader + validation
    homebox/            # REST client (entity model): list/create/patch/attach, tags, entity-types
    store/              # SQLite state (idempotency, backoff, change detection)
    discovery/          # SearXNG search -> rules filter -> LLM rerank -> confidence
    llm/                # OpenRouter client, modality-agnostic (text now, vision in Phase 2)
    notify/             # ntfy
    scheduler/          # Phase 1 cron loop
    portal/             # Phase 2 HTTP + camera intake
  config.example.yaml
  Dockerfile
  docker-compose.yml
```

## Conventions

- **Language: Go.** Single static binary, one image, in-process cron (container stays up, ~0 idle cost).
- **Core is packages, not a monolith.** Both stages share `internal/*` and one entrypoint
  (`serve`); the intake/curation boundary lives in the package graph, not in processes.
- **Config is a property file.** Everything schedule/threshold/tag-related is config, never hardcoded.
  Secrets (tokens/keys) come from env only — never from the config file, never committed.
- **Deployment lives elsewhere.** This repo ships source + Dockerfile + compose; site-specific
  deployment (Ansible, secrets injection, ingress) belongs to the deploying repo, not here.
- **Idempotent + cheap.** Diff, don't re-sweep. Cache negative results. Content-hash dedupe. Rate-limit egress.
