# Drydock

Agentic kanban — every card gets a container and a coding agent.

Drydock is the sibling of [TermDock](../termdock): same shape (single
self-contained Go binary, per-task Docker containers with persistent volumes,
embedded web UI, pluggable auth), but instead of interactive terminals each
kanban card is a unit of agentic work.

## How it works

```
ingestion → inbox → working → needs_you → qa → done
                 (auto-start)   (🔔 alarm)   (verifier)
```

1. **Ingestion.** Pluggable connectors create cards: `POST
   /api/ingest/webhook/:source`, RSS feeds (admin-configured, polled,
   deduped by guid), or manual creation. New cards land in the **inbox** and
   **auto-start** — no triage gate — unless the source has **LLM intake
   filtering** enabled: then each item is triaged by an LLM against the
   source's intake instructions, and only accepted items become cards.
   Rejected items are kept for 30 days in Admin → Rejected items, where you
   can tune instructions or accept one anyway.
2. **Working.** Each card gets a Docker container (named volume
   `drydock-card-<id>`, kept across restarts) and a worker agent runtime
   (`pi`, `zot`, …). The agent decomposes the task into steps — reusing the
   step plan of the most similar completed card from the same source when one
   exists — and a progress % badge ticks up as steps complete.
3. **Needs you.** When the agent is stuck it pauses and raises an **alarm
   bell** badge on the card. Clicking the card opens a messaging interface:
   you chat with the agent, answer its question, and it resumes.
4. **QA.** Finished cards go to a separate **verifier** agent pass. It can
   approve (→ done), send the card back to working with a note, or raise its
   own question to you.
5. **Done.** Verified work closes out: a markdown transcript (steps, questions
   with answers, chat, event log) is archived on the card in the database, and
   the card's container **and** its named volume are removed — no orphaned
   volumes. Moving a done card back to working starts it on a fresh empty
   volume, with the archived transcript posted as a system message for
   continuity.

Out of the box Drydock ships with the `echo` demo worker and demo verifier,
so the whole loop is exercisable with no LLM key. Real adapters implement
the `agents.Runtime` interface — see [docs/agents.md](docs/agents.md).

## Run it

```sh
go build -o drydock ./cmd/drydock
./drydock
# → http://localhost:8080
```

Register the first user (it becomes admin), and you're on the board.

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `DRYDOCK_ADDR` | `:8080` | Listen address |
| `DRYDOCK_DATA_DIR` | `./data` | Directory holding `drydock.db` |
| `DRYDOCK_DOCKER_HOST` | `DOCKER_HOST` / `/var/run/docker.sock` | Container runtime endpoint (mirrors `TERMDOCK_DOCKER_HOST`; bare socket paths work, Podman-friendly) |
| `DRYDOCK_DEFAULT_IMAGE` | `ubuntu:24.04` | Image for task containers |
| `DRYDOCK_AGENT_RUNTIME` | `echo` | Default worker runtime for new cards |
| `DRYDOCK_REG_OPEN` | `true` | Allow `/api/auth/register` |
| `DRYDOCK_TRUSTED_USER_HEADER` | *(unset)* | e.g. `X-Remote-User`: nginx does OAuth and passes the user in this trusted header; accounts auto-provision |
| `DRYDOCK_SECURE_COOKIE` | `false` | Set the session cookie `Secure` flag (behind TLS) |

When Docker is unreachable, cards still run — the agent works without a
container and the UI shows no container id. When a card is done, its
container **and** its named volume are removed together (the chat transcript
lives in the database, archived on the card); the card drawer's
"Remove container & volume" button does the same on demand with a confirm
dialog. Reopening a done card provisions a fresh empty volume.

## API

Boards: `GET/POST /api/boards`, `PUT /api/boards/:id` (admin),
`GET/POST /api/boards/:id/cards`
Wiki: `GET/POST /api/boards/:id/wiki`, `GET/PUT/DELETE /api/boards/:id/wiki/:pageID`
(cRUD admin-only for writes)
Cards: `GET /api/cards/:id`, `POST /api/cards/:id/messages`,
`POST /api/cards/:id/answer`, `POST /api/cards/:id/move`,
`POST /api/cards/:id/sendback`, `POST /api/cards/:id/stop`
Ingest: `POST /api/ingest/webhook/:source`
Admin: `GET/POST/DELETE /api/admin/feeds`, `PUT /api/admin/feeds/:id`,
`GET /api/admin/webhook-sources`, `PUT /api/admin/webhook-sources/:source`,
`GET /api/admin/rejected`, `POST /api/admin/rejected/:id/accept`,
`GET/POST/DELETE /api/admin/providers`,
`GET /api/admin/wiki-proposals?status=`, `POST /api/admin/wiki-proposals/:id/approve|reject`,
`GET/PUT /api/admin/settings`,
`GET /api/admin/volumes/orphaned`, `POST /api/admin/volumes/prune`
Live: `GET /api/stream` (board events), `GET /api/cards/:id/chat` (per-card chat)

## Design decisions

- **Auto-start, with optional LLM triage.** Ingested cards begin working
  immediately; the alarm badge + QA column are the control points, not an
  approval queue. Sources that need it (e.g. noisy email/RSS) can enable
  **intake filtering**: an LLM triages each item against the source's
  instructions and only accepted items become cards.
- **Filter is fail-open.** No provider configured, provider error, or an
  unparseable verdict all *accept* the item with a `filter unavailable`
  reason — inbound work is never silently dropped. The Admin → Rejected
  view shows a hint when recent cards were accepted this way.
- **Rejected items are reviewable.** Declined items are kept 30 days with
  the filter's reason; accepting one anyway creates + auto-starts its card.
  This is the tuning loop for writing good intake instructions.
- **Steps are the progress bar.** The % badge is always `done/total` steps —
  runtimes report step completions, they don't invent percentages.
- **History-driven planning.** New cards clone the step plan of the most
  similar completed card from the same source (Jaccard over title keywords,
  threshold 0.25), so repeat task types get better plans for free.
- **Separate verifier.** QA is a different agent pass, not the worker
  self-grading — it can send work back or escalate to the user.
- **Card chat is the intervention UI.** Questions, answers, and free-form
  messages all live on the card; the agent resumes in context.
- **Webhooks sit behind auth** in this build (no per-source secrets yet) —
  call through the same reverse proxy that enforces auth.
- **No orphaned volumes.** A card's volume never outlives its container:
  reaching done (or the manual remove button) deletes both, after archiving
  a full markdown transcript onto the card in the database. Volumes orphaned
  *before* that policy existed (card gone, or card done) can be found and
  deleted from Admin → Volumes — the scan only ever lists volumes whose
  card is gone or done, and prune re-verifies orphan status at delete time
  (a done card reopened between scan and prune gets a fresh volume under the
  same name, which must not be touched) and deletes exactly the checked
  names, best-effort per volume.
- **Flows: the pipeline is visual.** Each board owns one Node-RED-style
  flow (hand-rolled SVG editor, no dependencies) that controls how inbound
  items travel through LLMs to outputs. The previous hardcoded pipeline is
  the seeded *default flow* (`inbox → worker`, `manual → worker`), so
  behavior is unchanged until a board customizes it. Node palette v1:
  `inbox` / `manual` inputs, `llm_filter` (accept/reject ports, rejects go
  to the Rejected queue), `llm` (prompt templates with
  `{{title}}`/`{{body}}`/`{{var:name}}`, output to body or a var), `worker`
  (card creation + autostart, optional runtime override and verify toggle),
  `notify` (webhook POST on card close). The engine compiles (cycle/port/
  config validation), walks the DAG synchronously per item, and logs every
  run for observability. See `docs/flows.md`.
- **Single static binary**, `go:embed` UI, SQLite (WAL), no CGO — same
  deployment story as TermDock.
- **Instructions layer.** Global working instructions plus per-board
  instructions are injected into every worker run as system context
  (`RunContext.Briefing`), so agents work the way the user asked.
- **Persistent per-board wikis.** Markdown pages agents read at run start
  (capped ~12KB in the briefing). Agents propose edits via a `wiki_proposal`
  event; proposals sit in an admin **approval queue** — nothing applies
  without approval, and every applied edit (user or approved agent) is kept
  as a revision with `user:<name>` / `agent:<card_id>` attribution.

## Build notes

No system Go on the dev VM: `export PATH=~/go1.26/go/bin:$PATH
GOCACHE=~/workspace/.gocache`. Dependency versions mirror TermDock
(docker v28.5.2, gorilla/websocket, modernc.org/sqlite).
