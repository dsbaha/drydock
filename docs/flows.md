# Flows — Node-RED-style visual pipelines

Each board owns one **flow**: a visual DAG that controls how inbound items
travel through LLMs to outputs. The pipeline that Drydock shipped with
(intake filter → create card + worker → verifier → done) is the *default
flow* — a board with no custom flow behaves exactly as before.

## Concepts

- **Input nodes** start a run. `inbox` fires for every item accepted by
  intake filtering (RSS, webhooks); `manual` fires for the "New card" button.
  At most one of each per flow.
- **Processor nodes** transform or route the envelope `{item, card_id, vars}`:
  - `llm_filter` — LLM triage against the node's `instructions`. The
    `accept` port continues the walk; `reject` records the item in the
    existing `rejected_items` table (kind `flow`) and ends the path. Its
    `reject` port may be left unwired. Fail-open: with no classifier or LLM
    provider, the item is accepted and the card is annotated.
  - `llm` — rewrites the item via the configured LLM provider. `prompt`
    supports `{{title}}`, `{{body}}` and `{{var:name}}` placeholders.
    `output` is `body` (replace the item body) or `var:<name>` (stash in the
    envelope for later prompts).
  - `worker` — creates the card and autostarts the agent (this is where card
    creation lives). `runtime` overrides the board default (empty = default);
    `verify: false` skips the QA pass when the worker finishes. Its `out`
    port is optional and fires right after the card is created.
- **Output nodes**: `notify` registers a close-hook: when the card hits done,
  Drydock POSTs `{card_id, board_id, title, transcript}` to `url`
  (best-effort, 10s timeout, then the hook row is deleted). A notify node
  must come after a worker on every path — a hook needs a card.

## Engine semantics

- `internal/flows`: `Compile` validates the definition; `Engine.Trigger`
  loads (or seeds) the board's flow, compiles it, and walks from the matching
  input node(s) in topological order, carrying the envelope.
- The walk is **synchronous**; the worker node is fire-and-forget (card
  creation + autostart happen in the orchestrator, the walk continues
  immediately). There is no durable suspension in v1.
- Every trigger is recorded in `flow_runs` (`ok` | `rejected` | `error`,
  30-day retention) for observability in the editor's Recent runs list.
- The existing intake-level LLM filter runs *before* the flow, unchanged;
  flow-level `llm_filter` nodes are for additional in-flow branching.

## Validation rules

At least one input node; at most one per kind; known node types; edges
reference real nodes/ports; at most one wire per input port; no cycles;
every non-input's `in` connected; every `out`/`accept` connected (except
`llm_filter.reject` and `worker.out`, which may dangle); every maximal path
ends at `worker` or `notify`; every path to `notify` passes through a
`worker`; per-type config schema (`instructions`, `prompt`, `body|var:name`
output, http(s) `url`).

## API

- `GET /api/boards/{id}/flow` — the board's flow (seeds the default; any user)
- `PUT /api/boards/{id}/flow` — replace (admin; 400 with an `errors` list when it doesn't compile)
- `POST /api/boards/{id}/flow/reset` — reseed the default (admin)
- `GET /api/admin/flow-runs?board_id=` — recent runs (admin)
- `GET /api/admin/runtimes` — registered agent runtimes for the worker config (admin)

## Writing a custom runtime

Runtimes are registered on the orchestrator (`RegisterRuntime`) and appear
in the worker node's runtime picker. If a flow names a runtime that isn't
registered, the orchestrator falls back to `echo`. A runtime that wants the
run's system context (instructions + wiki pages) must include
`RunContext.Briefing` — see `docs/agents.md`.
