# Knowledge intelligence

Knowledge intelligence is optional. The default mode is `disabled`: existing
`Remember`, conversation extraction, Search and Context keep their behavior.
No external model is enabled by default.

## Architecture

```
input + evidence -> bounded state lookup -> optional planner
  -> deterministic validation -> Reconciler -> store.Write(Batch)
                                           -> durable leased jobs
```

The planner proposes; it never writes. Authorization, tenant boundaries, trust,
version checks and temporal validation remain deterministic. Retrieval never
calls the transition planner or a generative LLM. Embedding models remain an
independently optional retrieval component.

State lookup prioritizes explicit target IDs, slots and subject/predicate
matches, then existing hybrid retrieval and authorized relationship neighbours.
It does not interpret missing vectors as evidence that no related knowledge
exists. The default candidate bound is 16 and graph depth is two; server limits
are 64 candidates and four hops.

## Structured records

Text-only memories remain valid. `Fact.Assertion` and `Fact.Decision` encode
optional structured knowledge into the reserved `assertion` and `decision`
metadata keys. This uses existing metadata persistence; it does not require
rewriting historical text-only records.

Assertions contain subject, predicate, object, epistemic type, conditions,
confidence and evidence memory IDs. Epistemic types are `fact`, `observation`,
`belief`, `hypothesis`, `decision` and `preference`. They are independent of
`Kind` and the Atom/Scene/Profile layers. Conditions are bounded maps, not an
unrestricted expression language.

Decision metadata distinguishes proposed, active, superseded, retracted and
unresolved decisions. A model recommendation does not establish authority.
Confidence is not authorization. Episode evidence identifies an accessible
episode, part and byte span; assertion/decision evidence IDs identify accessible
memory records.

Structured hashing uses canonical serialization, including conditions.
Text-only hashing remains compatible with existing records.

## Transitions

| Operation | Meaning |
|---|---|
| `ADD` | Independent assertion; cannot implicitly overwrite an occupied slot. |
| `REINFORCE` | Append an evidence event without changing the original assertion. |
| `SUPERSEDE` | World changed: end the previous validity interval and add its successor. |
| `CORRECT` | Earlier assertion was wrong: invalidate it at knowledge time and add a corrected version. |
| `REFINE` | Add a linked refinement, preserving the independently valid original; replacements use `CORRECT` or `SUPERSEDE`. |
| `RETRACT` | Auditable withdrawal; retain the original record. |
| `BRANCH` | Preserve both assertions; the conditional branch is not an unconditional scalar slot replacement. |
| `RELATE` | Add validated relationships between accessible existing records. |
| `DEFER`, `IGNORE` | No speculative knowledge mutation. |

Use explicit intent and evidence rather than relying on wording heuristics.
The deterministic planner handles explicit operations and obvious duplicates.
Semantic interpretation requires an explicitly configured planner.

Retries reload and revalidate current state. Transactional version checks prevent
a stale model plan from changing a version another worker already closed.
Multiple operations targeting the same version in one plan are rejected.

## Atomicity and recovery

Memory versions, temporal closes, relationship additions/retirements, evidence
events, dependency notifications and operation results share the existing
`store.Write(Batch)` transaction. There is no separate relationship write API.
Jobs appended to that batch are the durable outbox, using the existing leased
queue rather than another job system.

Knowledge commits require a stable `input.operation_key`. Repeating a key with
the same input returns the stored result without another transition; using it
with different input is a conflict. Validated planning results are persisted
before commit and reused when the candidate state has not changed. A crash
between planning and application therefore does not require another model call.

Existing HTTP facts, updates, invalidations and deletions accept
`Idempotency-Key`. Go callers can supply `Fact.OperationKey`, `Patch.OperationKey`,
`InvalidateWithKey` or `DeleteWithKey`. Legacy calls remain valid; a generated
per-call key protects internal retries, but a caller must retain its own key
for retries across requests.

Historical assertion text and original source evidence are not replaced by an
intelligent correction. Reinforcement evidence is associated in append-only
history events. Relationship replacement retires earlier edges and appends new
ones rather than erasing their history. Explicit privacy deletion/Forget remains
different from correction: it removes content and redacts affected operation
results while retaining idempotency tombstones.

## The three temporal perspectives

`ValidFrom`/`ValidTo` describe the world. `RecordedAt`, `ClosedAt` and
`InvalidatedAt` describe what the system learned and when.

Example: Lisbon was recorded on day one. On day five, the engine learns Alice
moved to Porto on day three.

| Query | Result |
|---|---|
| `known_at=day2`, `valid_at=day4` | Lisbon: what was believed before learning the move. |
| `known_at=day6`, `valid_at=day2` | Lisbon: currently believed about the earlier world state. |
| `known_at=day6`, `valid_at=day4` | Porto: currently believed about the later world state. |

A later correction preserves the original assertion for `known_at` before the
correction, while current knowledge of that same past period uses the corrected
version. A validity end is ignored for earlier knowledge times until `ClosedAt`.
Legacy records with no closing-knowledge timestamp retain their legacy temporal
interpretation; the system cannot reconstruct a timestamp never recorded.

These distinctions are exercised by shared engine conformance tests.

## Relationships and visibility

Relationships include ID, tenant, endpoints, type, confidence, evidence,
provenance, validity, recording time and retirement time. Supported knowledge
types include `supports`, `contradicts`, `supersedes`, `refines`, `depends_on`,
`derived_from` and `exception_to`; inference also supports `extends` and
`related_to`.

`KnowledgeRelations` supports direction, type filters, temporal queries and
bounded depth/edge counts. Every endpoint is rechecked against tenant, ACL,
bindings and requested scope. A readable team node never reveals a private
neighbour. Cycles terminate through a visited-node set.

Optional background relationship inference uses the separate `relations.Inferer`
interface and `connectors/inference` module. This annotates existing records;
it does not authorize corrections. Model estimates are explicitly labelled
`origin=inferred`, distinct from `validated` transitions and `derived` collections.

The developer explorer shows bounded graph slices, provenance and version links.
The timeline separates world-time boundaries from knowledge-time discoveries.
Neither view claims to be an exhaustive audit dump.

## Providers and configuration

```json
{
  "knowledge": {
    "mode": "shadow",
    "planner": {"use": "deterministic"},
    "max_candidates": 16,
    "max_graph_hops": 2,
    "timeout_ms": 2000,
    "fallback": false,
    "consolidation": false
  }
}
```

Merge this section with the usual store, authentication and worker configuration,
or start from [the complete example](../configs/knowledge.example.json).

Modes:

- `disabled`: no intelligence interpretation or semantic transitions. With runtime
  profiles registered, ordinary writes can still enqueue jobs; workers check the
  effective tenant mode and explicitly skip disabled processing.
- `shadow`: persist a commit's proposed/validated plan, but no semantic transition.
- `enforced`: apply validated transitions through the Reconciler.

`EvaluateKnowledge` remains side-effect-free in either enabled mode.
Provider selection follows the existing Choice convention:

```json
{
  "planner": {
    "use": "ollama",
    "ollama": {
      "base_url": "http://localhost:11434",
      "model": "your-installed-model"
    }
  }
}
```

`openai` selects an OpenAI-compatible chat-completions provider with configurable
`base_url`, `api_key` and `model`. The optional
`github.com/alexedtionweb/niro-memory/connectors/knowledge` module implements both
protocols. Core applications may implement `knowledge.Planner` directly.
No Laya API is fabricated or required.

For Qwen3 on Ollama, [the local Qwen3 configuration](../configs/qwen3.example.json)
uses `qwen3:0.6b`, enforced mode and `"think": false` to avoid spending the bounded
planner budget on extended reasoning. The optional Ollama `think` boolean
preserves the model default when omitted. Qwen3 is a chat planner, not an embedder;
the example separately configures `nomic-embed-text` (768 dimensions, query/document
prefixes) for semantic retrieval. Install both models in Ollama first. The example
enables background enrichment: originals save immediately and extraction runs
as durable work. Advanced knowledge transitions still require confirmation.
Use `shadow` to inspect extraction without applying interpreted claims.

### CPU-first background enrichment

Set `knowledge.background_enrichment` to `true` and select a planner implementing
`knowledge.Enricher` (the Ollama/OpenAI knowledge connector implements both
interfaces). This is an opt-in alternative to asynchronously asking the older
operation planner to reinterpret a whole input as one memory.

1. `/v1/facts` / `Remember` (including MCP `remember_memory`) store the original
   and enqueue `knowledge.enrich` in the **same `store.Write(Batch)`**. No model or
   embedding runs on this save path. Supply a stable operation key on retries.
2. Conversation append durably schedules ordinary processing as before. A
   provider-free source extractor preserves non-system text parts with exact
   episode/part evidence; cursor advancement, source writes and enrichment jobs
   are atomic. Structured-only/media-only parts still need a configured extractor;
   they are not silently presented as model-extracted text.
3. The worker performs one bounded structured extraction, not separate calls
   for each fact, entity, decision and relationship. Ollama runs with thinking
   disabled, CPU-only (`num_gpu: 0`), temperature zero, a constrained JSON schema,
   four inference threads, 4096 context tokens, at most 700 output tokens and a
   ten-minute keep-alive.
   The example sets a 15-second per-request timeout. Other planner calls retain
   their existing settings.
4. At most four quoted claims and eight links are admitted. Sources over 6000
   bytes fail explicitly, without losing the saved original. At most six recent
   current candidates (also bounded by the tenant candidate limit), each at most
   1000 bytes and 2000 bytes total text, are selected from a 64-row scoped window.
   This is a bounded context, **not an exhaustive semantic conflict search**.
5. Quotes must exist verbatim in the saved source. Subjects/objects must occur
   in the quote; references, epistemic types and link types are validated.
   Explicit negation is stored as assertion condition `polarity: negative`;
   affirmative claims use `positive`. Exact unconditional structured claims
   with opposite polarity also generate deterministic potential-conflict links.
   Different values or conditions alone never trigger this rule.
   Explicit extracted English decision predicates (`decided`, `chose`, `selected`
   and their present-tense forms) classify a generic model `fact` as a proposed
   decision. The model's original classification is retained as `model_type`.
   Other languages depend on the provider's epistemic classification.
   Entity references preserve extracted names; they do not claim canonical
   cross-language resolution or invented entity classes.
6. The exact candidate snapshot and extraction are durably receipted before
   application. Stable source-version IDs, operation keys and optimistic checks
   let another node resume without calling the model again or duplicating
   transitions. If evidence/context closes, the result requires review instead
   of forcing a stale transition.
7. New assertion versions, entity indexes, `derived_from` evidence edges,
   inferred connections, audit events, embedding work and the completion receipt
   commit atomically. The original memory/assertion is never rewritten with a
   new interpretation. Dependency notifications remain part of the store
   transaction when supporting memories close.

`enforced` applies these **unreviewed annotations**, not authoritative corrections
or retractions. Extracted decisions start as `proposed`. Confidence is a fixed
conservative admission value (0.5), not a calibrated model probability. Trust,
scope, visibility, grants, expiry and world-validity interval inherit from the
source; recorded time is the new interpretation's recording time. A potential
`contradicts` link is a review signal, not proof either assertion is false.
Admission decisions and disabled/shadow modes remain effective. Shadow mode
saves the source but persists no extracted memories or edges. Previously completed
shadow extractions are not automatically applied after a mode switch.

Inspect real progress through:

- `GET /v1/knowledge/enrichment/{source-memory-id}` (source-authorized);
- MCP `inspect_enrichment`, with the source ID as `key`, restricted to its fixed
  project scope;
- **Save now** and memory detail pages in the Dev UI, with automatic status
  refresh, error display, original/extracted memory links and raw evidence.

States distinguish queued, processing, completed, shadow, disabled, failed,
admission decisions, needs-review and not-scheduled. Failed/retrying jobs expose
their actual queue error. Pending work and results are durable, not process-local
UI state. Older memories are not falsely reported as already enriched.

To reproduce a synthetic CPU quality/latency comparison against local Ollama:

```sh
go run ./connectors/knowledge/testdata/cpu-benchmark.go qwen3:0.6b qwen3:1.7b > cpu-results.json
```

Each model receives one separately reported warm-up and twenty measured requests
covering a fact, a decision, a greeting and an explicit conflict with a structured
existing assertion. The conflict score includes deterministic polarity matching;
it is not a measure of model-only relationship inference. Output includes
every response/error, schema/grounding validation count, simple case-pass count
and nearest-rank p50/p95. These small synthetic samples are **not** production
accuracy or latency guarantees. Inspect actual claims; passing grounding checks
does not establish semantic truth. A fast model returning empty arrays is not
counted as successfully extracting a substantive claim.

Local exploratory runs tested `gemma3:270m`, `smollm2:360m`, `qwen2.5:0.5b`,
`qwen2.5:1.5b`, `qwen3:0.6b` and `qwen3:1.7b`. The smaller Gemma/SmolLM and
Qwen2.5 trials had frequent grounding/format failures; the larger Qwen trials
were slower. Prompt/schema iterations were part of these trials, so they are
not a controlled leaderboard.

The selected four-thread `qwen3:0.6b` pipeline, measured without concurrent test
execution on the local Core Ultra 7 165U CPU, produced:

| Warm requests | Valid/grounded outputs | Synthetic case checks | p50 | p95 |
|---|---|---|---|---|
| 20 | 20/20 | 20/20 | 2395 ms | 3607 ms |

Separately, twenty new `/v1/facts` HTTP writes against the running SQLite server
with background processing enabled measured **6.2 ms p50 / 8.2 ms p95** on
localhost. Live read-back verified named entities, an explicit negative assertion
with a potential-conflict edge, and a proposed decision retaining the original
model classification. All synthetic verification memories were then removed
from their separate test tenant.

The extraction samples are five repetitions of four small cases, not twenty independent knowledge
domains. A simultaneous race-test workload caused timeouts in a separate run:
CPU contention, cold loading, longer sources, queue delay and model mistakes
still matter. The architecture removes model latency from saving; it does not
promise every enrichment completes within these observed percentiles.

External planners receive bounded candidate memory content. Configure them only
when that data disclosure is approved. Responses are strictly decoded and
validated; unknown operations, invalid references, excessive output and malformed
confidence values are rejected. No tools or unrestricted chain-of-thought are
executed or stored.

Without an explicit runtime opt-in, per-tenant settings may only reduce
mode/candidate/depth limits through `Settings.Knowledge`. Provider credentials
remain server configuration. The development console runtime opt-in is described
below; candidate/depth limits can never exceed startup bounds.

Optional `knowledge.admission` accepts `enabled` and `max_text_bytes` (32 KiB
default, at most 1 MiB). It controls background interpretation only; an explicit
Remember has already been durably stored and is never probabilistically dropped.
Go applications may supply their own `knowledge.AdmissionDecider`.

## Dev UI workflows

Start the existing single-process stack:

```sh
go run -tags devui ./cmd/niromem serve -dev-ui -mcp -mcp-user my-project
```

Open `http://localhost:8080/_dev/`. Existing admin authentication, API key
selection and tenant isolation apply to every knowledge request.

| Workspace | Real backend data and actions |
|---|---|
| Memories (`#/explorer`) | Cards-first browsing, selected-memory history and connections. Older versions, the bounded graph and model diagnostics are optional. |
| What is known? (`#/knowledge-state`) | Project/topic search with readable cards; expand dates or advanced filters for historical and exact-match queries. |
| Saved result (`#/transitions`) | Plain-language outcome for an exact operation key, suggested actions and changed-memory links. Technical validation/proposal/receipt details are collapsed. |
| Try a memory (`#/intelligence`) | Background mode: save original → follow extraction → inspect claims/connections. Advanced transition preview/confirmation remains available. JSON, settings and retry keys are under Advanced. |

### Preview, validate and commit

With background enrichment enabled, **Save now** writes the original immediately.
Its status panel follows actual queued/processing/completed or failed work, and
links to the extracted memories. Shadow/disabled affect extraction, not explicit
source saving. No model preview is required.

For the advanced transition-preview workflow (the default without background
enrichment):

1. Open **Try a memory**. The banner names the active model and whether it is
   **Test mode** (`shadow`), **Live mode** (`enforced`) or **Paused** (`disabled`).
   The visible **Decision planner** and **Embedding system** panels distinguish
   configured models from actual embedding coverage. **Refresh model status**
   re-reads backend state; configuration alone does not establish model health.
2. Write plain text and optionally a project/user name. Selecting **Suggest a
   correction** from an existing memory preserves its target and scope.
   For structured assertions, decisions, evidence, intent or validity, open
   **Advanced: input, evidence & retry key** and explicitly enable **Use JSON
   instead of the form**. Turning JSON off restores the form values; there is
   never a silent merge of two competing inputs.
3. **See what happens** calls the read-only evaluate and state APIs. The result
   says **Preview only — nothing saved**, explains the suggested action in plain
   language and reports safety checks. Raw provider proposals and validated
   operations remain under Advanced. Preview changes no memory/heat or trace.
4. Review the suggestion, check the confirmation, then **Record test result**
   in test mode or **Save to memory** in live mode. Test mode records the proposal
   but leaves memories unchanged. Live mode reloads and revalidates state, so
   the final result may differ from the preview. The outcome distinguishes
   saved memory changes from a recorded result with no changed memory versions.
5. After any commit attempt, the exact input and stable key are locked for retries,
   including lost responses. A failed state refresh is explicitly distinguished
   from a failed commit. **Retry / retrieve this result** keeps the original
   key and payload. **Start over** creates a new key and unlocks editing; do not
   use it to retry an unconfirmed write.
6. Follow **Open this saved result** or memory/history links for deeper inspection.
   **Compare memories before & after** expands the session snapshots.
   **Advanced: model & settings** retains runtime provider and mode selection;
   other debugging pages remain available under **Developer tools**.

Before/after snapshots are session-only bounded reads, not transactionally
captured full-store snapshots. Concurrent writes may appear in the subsequent
state query. Durable trace enumeration and original input snapshots are not
implemented. Older receipts may lack proposals, validation or timing metadata;
the UI labels missing fields as unrecorded rather than reconstructing them.
Validation failures record structured validation diagnostics when available.
No provider chain-of-thought or mock intelligence is displayed.

### Historical perspectives

- Both clocks empty: current state.
- **Valid at** and **Known at** set: what was believed then about that world time.
- **Valid at** set, **Known at** cleared: current beliefs about that past period.

Datetimes entered in the UI use the browser timezone and are sent as RFC3339.
The “Other versions” section can include versions not yet recorded
at the selected knowledge time; these are not presented as current beliefs at
that time. Candidate bounds still apply, so absence from this view is not proof
that the database contains no other matching evidence.

### Durable runtime provider profiles

The CLI registers runtime profiles **only with `-dev-ui`**, after validating its
development/loopback restrictions. This explicitly allows tenant admins to
activate `shadow` or `enforced` even if startup mode is `disabled`. It does not
change the global startup mode or any other tenant. Production without this
opt-in retains the original narrowing-only policy.

`deterministic` selects the built-in rules planner; `configured` selects the
startup planner. Add named profiles under the normal configuration's
`knowledge.profiles`:

```json
{
  "knowledge": {
    "mode": "disabled",
    "planner": {"use": "deterministic"},
    "profiles": {
      "local-model": {
        "use": "ollama",
        "ollama": {"base_url": "http://localhost:11434", "model": "your-installed-model"}
      },
      "approved-chat": {
        "use": "openai",
        "openai": {
          "base_url": "https://your-approved-endpoint/v1",
          "model": "your-approved-model",
          "api_key": "${KNOWLEDGE_API_KEY}"
        }
      }
    }
  }
}
```

Selecting a profile is a runtime change, not a server restart. Installing new
providers or changing profile endpoints, credentials, models, timeout, fallback,
admission or consolidation configuration requires a server configuration change
and restart. No credentials or endpoints are returned by
`GET /_dev/api/knowledge/config`. External profiles must only be registered when
disclosure of the tenant's candidate memories to that provider is approved.

Selection is stored through `PUT /v1/settings` as
`knowledge: {"mode":"shadow","planner":"local-model","max_candidates":8,"max_graph_hops":1}`.
Zero/omitted limits inherit startup bounds. Other nodes observe changes within
their settings cache TTL. Go integrations can explicitly register the same
planner map using `Options.KnowledgePlanners`; **all nodes and workers sharing
these tenant settings must register the same profiles**, including after restart.
An unavailable profile is an explicit error, never a silent provider fallback.

## Background consolidation and dependencies

With enforced consolidation enabled, leased jobs build bounded Scene collections
of Atoms, then Profile collections of Scenes. The baseline intentionally creates
transparent source collections, not invented semantic summaries. Every collection
preserves source IDs and `derived_from` edges, with no visibility broadening.

Closing a source transactionally queues dependent `knowledge.reevaluate` jobs.
Dependency traversal includes transitive dependents, stops on cycles, and is
bounded to four hops and 128 jobs per batch. If truncated, jobs carry
`truncated=true`, workers log a warning, and review events explicitly mark that
additional investigation is required.
Processing records an auditable `dependency_changed:needs_review` notification,
not an automatic deletion or an ungrounded correction. Operators can inspect
the dependency and explicitly commit a validated revision. This baseline does
not claim automatic model-driven repair of arbitrary summaries.

## HTTP and MCP

All routes use existing API authentication and tenant/scope binding:

| Route | Behavior |
|---|---|
| `POST /v1/knowledge/evaluate` | Interpret and validate without writes. |
| `POST /v1/knowledge/plan` | Side-effect-free planning alias. |
| `POST /v1/knowledge/commit` | Reload state, validate, and commit using a stable key. |
| `GET /v1/knowledge/state` | Bounded authorized knowledge lookup. |
| `GET /v1/knowledge/relations/{id}` | Authorized relationship traversal. |
| `GET /v1/knowledge/traces/{key}` | Durable result/failure classification. |

A commit request, with server-bound tenant/authority:

```json
{
  "input": {
    "operation_key": "decision-001",
    "scope": {"user_id": "project"},
    "text": "Use PostgreSQL for production",
    "trust": "high",
    "intent": "ADD",
    "assertion": {
      "subject": "production",
      "predicate": "database",
      "object": "PostgreSQL",
      "epistemic_type": "decision",
      "confidence": 1
    }
  }
}
```

Clients cannot submit a trusted `validated=true` flag or use confidence to gain
authority. Mutation API authority is established server-side.

The original MCP tools `search_memory` and `remember_memory` retain their required
arguments. Optional `inspect_knowledge` and `explain_memory` inspect state/traces;
`propose_memory_update` is dry-run only. MCP identities remain bound to the
server's configured project and authenticated tenant.

## End-to-end decision evolution

1. Commit an authorized `ADD`: production database is PostgreSQL.
2. Record a benchmark observation independently; relate it with `contradicts`
   rather than overwriting the decision.
3. Use `BRANCH` for SQLite under `{"environment":"development"}`. Both conditional
   assertions remain represented.
4. When an authorized decision actually changes, use `SUPERSEDE` with a later
   `valid_from` and the current `target_id`.
5. Query the old world period with current `known_at`, then with the earlier
   `known_at`, to distinguish historical validity from historical belief.
6. Inspect version history, typed relationships and the operation trace to
   explain evidence, reason codes, provider, and applied changes.

Use `CORRECT` instead of step four when the earlier assertion was never right.
Ambiguous recommendations should remain hypotheses or `DEFER`, not active
decisions.

## Failure behavior and operations

Planner timeouts and validation failures leave existing memory intact and persist
a failure classification for commits. Deterministic fallback is opt-in and
explicitly marked. Successful commits, traces, receipts and jobs are tenant-scoped.
Worker retries and lease recovery use the existing queue.

SQLite and PostgreSQL use additive schema upgrades for relationships, operation
receipts and closing-knowledge timestamps. Existing memory metadata stays valid.
Use normal backups before upgrading a database.

Operational limits: relationship reads return at most the newest 4,000 matching
rows and do not yet offer history pagination. PostgreSQL writes serialize per
tenant; relationship mutation planning and receipt redaction inspect tenant
history. Benchmark these paths before high-volume deployment.

Run focused validation:

```sh
go test -race ./... ./connectors/sqlite/... ./connectors/postgres/...
go test ./connectors/knowledge/... ./connectors/inference/...
go test -tags devui ./devui ./cmd/niromem
node --test devui/explorer-model.test.mjs devui/knowledge-ui.test.mjs
node --test devui/explorer-model.test.mjs
```

PostgreSQL integration tests require their configured test DSN. HTTP-fixture tests
do not certify real-model reasoning quality.

Benchmarks in `knowledge_bench_test.go` use 200 in-memory text records and a
relationship chain. They separately measure bounded lookup, deterministic
validation, depth-two traversal and Search with relationship expansion disabled
and enabled. The reconciliation benchmark includes a fresh store/engine, an
initial write, and a correction transaction with its relationship and receipts.
These are microbenchmarks, not production latency guarantees.
Run `go test -run '^$' -bench BenchmarkKnowledge -benchmem .` on your own workload.
