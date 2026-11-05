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
3. The worker starts with one bounded structured extraction, not separate calls
   for each fact, entity, decision and relationship. Ollama runs with thinking
   disabled, CPU-only (`num_gpu: 0`), temperature zero, a constrained JSON schema,
   four inference threads, 4096 context tokens, initially 1200 output tokens and a
   ten-minute keep-alive by default. Transition planning now also uses bounded
   CPU generation (512 output tokens by default), rather than Ollama's unbounded
   generation defaults. Both paths accept planner options `num_thread`, `num_ctx`
   and `num_predict`; zero selects the defaults.
   The provider protocol is model-independent: the server splits source text
   into numbered, verbatim evidence spans of at most 2048 bytes. Models select
   an `evidence_id` for each fact/entity rather than generating quotation text.
   Ollama's constrained schema bounds the IDs; OpenAI-compatible requests carry
   the same schema and examples in the prompt. The server resolves IDs back to
   exact source text before running the unchanged grounding/type validators.
   IDs never refer to neighboring candidates. Invalid IDs, conflicting quotes
   and subject/object/entity names absent from the selected evidence are rejected.
   Legacy verbatim-quote responses remain supported and fully validated.
   This reduces copying/paraphrasing failures, not semantic hallucinations:
   a valid evidence reference alone does not prove the predicate is correct.
   The effective extraction profile's public schema still describes the resolved
   quote-based result, while provider requests use this indexed-evidence schema.
   When an extraction unit has no usable records because validation failed, the connector
   makes at most one source-only correction with validation feedback within the same request deadline. It
   records the rejected validation error in normalization notes, revalidates
   every quote, and resolves deterministic polarity links against the original
   candidate indexes. It also works without candidates, and does not
   retry transport failures. Valid records with rejected optional annotations
   are retained without another model call. This avoids repeating a noisy context without accepting
   ungrounded output. A second invalid response still fails explicitly.
   Incomplete generation (including truncated JSON and provider token-limit
   finish reasons) is a retryable provider failure, not invalid source evidence.
   An initially truncated Ollama extraction gets one immediate retry, doubling
   its output budget up to 4096 tokens within the original request deadline.
   It never repairs or accepts partial JSON; continued truncation remains a
   durable job retry rather than an immediate permanent rejection.
   Grounding is not semantic correctness: the small CPU model can still assign
   an incorrect entity category or epistemic type, or miss a claim entirely.
   A completed job is not proof of correct understanding. Inspect the returned
   claims and unreviewed annotations, not just queue state or HTTP success.
   The Qwen and LFM3-Omni structured examples start at 1536 output tokens.
   Qwen allows twenty minutes per source extraction and LFM3-Omni ten
   for cold loading and CPU inference. The provider HTTP
   client and worker deadline do not impose shorter caps. Set
   `knowledge.timeout_ms` (1..1800000) for your hardware; restart both server and
   external workers after changing it. Original saves do not wait for inference.
   Worker concurrency is configurable; begin with one on CPU-only machines.
   Increasing it can queue multiple inference requests behind the local model.
   On a two-core host,
   set `num_thread` to 2. More workers can increase CPU contention rather than
   throughput. These limits do not serialize other applications using Ollama.
   Pending legacy `knowledge.evaluate` jobs use source-keyed enrichment when
   background enrichment is enabled, sharing its durable receipt with newly
   queued `knowledge.enrich` jobs instead of running the older planner again.
   After restarting with the new configuration, retry dead jobs from Operations.
   Retry reuses the same job ID instead of copying a second visible job; it resets
   attempts/checkpoint. Pause/resume, cooperative cancellation, inactive deletion
   and priority controls are documented in [Operations](./operations.md).
   Invalid model output still fails explicitly; no invented
   evidence is admitted just to make the queue look healthy.
   The Dev UI disables manual worker passes while the inline worker is active;
   the backend enforces this too, so pressing the button cannot bypass the CPU
   concurrency limit. Without an inline worker, a manual pass claims one job,
   not a large batch whose leases could expire before execution.
4. Each model call admits at most four quoted claims, twelve named entities and
   eight links. Independent evidence windows aggregate up to 64 claims and
   320 entity annotations (including inline annotations), with at most eight candidate links.
   Sources over 6000
   bytes fail explicitly, without losing the saved original. At most six recent
   current candidates (also bounded by the tenant candidate limit), each at most
   1000 bytes and 2000 bytes total text, are selected from a 64-row scoped window.
   This is a bounded context, **not an exhaustive semantic conflict search**.
5. Quotes must exist verbatim in the saved source. A model's final `.`/`!`
   mismatch (including an added final punctuation mark absent at the end of the
   source) may be aligned only to a unique otherwise-exact source span, with
   the original model quote and the adjustment explicitly recorded in validation
   notes. Questions, changed words and ambiguous matches are never repaired.
   If a provider echoes an existing candidate as an extra claim, that exact
   candidate quote is excluded with a validation note only when at least one
   valid source-grounded claim remains. Unknown quotes, candidate-only outputs
   and relationships referencing a discarded context claim are rejected.
   Subjects/objects must occur
   in the quote; references, epistemic types and link types are validated.
   Explicit negation is stored as assertion condition `polarity: negative`;
   affirmative claims use `positive`. Exact unconditional structured claims
   with opposite polarity also generate deterministic potential-conflict links.
   Different values or conditions alone never trigger this rule.
   Explicit extracted English decision predicates (`decided`, `chose`, `selected`
   and their present-tense forms) classify a generic model `fact` as a proposed
   decision. The model's original classification is retained as `model_type`.
   Other languages depend on the provider's epistemic classification.
   Entity names and evidence must appear in the source; types and optional
   attributes must satisfy the tenant profile. Types are model annotations,
   not verified identities or canonical cross-language resolution.
6. The exact candidate snapshot, extraction profile, output schema and extraction are durably receipted before
   application. Stable source-version IDs, operation keys and optimistic checks
   let another node resume without calling the model again or duplicating
   transitions. If evidence/context closes, the result requires review instead
   of forcing a stale transition.
7. New assertion versions, entity indexes, `derived_from` evidence edges,
   inferred connections, audit events, embedding work and the completion receipt
   commit atomically. The original memory/assertion is never rewritten with a
   new interpretation. Dependency notifications remain part of the store
   transaction when supporting memories close.

   #### Duplicate prevention and evidence

   Background extraction reconciles canonical structured assertions instead of
   unconditionally inserting a memory per source. Repeated equivalent assertions
   produce an `evidence` change referring to the existing memory, an append-only
   audit event and a source-specific `derived_from` edge. The original assertion,
   quote, entity annotations, recording time and provenance are not rewritten.
   Evidence sources remain separately inspectable: retaining an original source
   and its interpreted assertion is intentional, not failed deduplication.
   The Dev UI labels these cards **Original input** and **Model extraction**
   (including the stored review status), rather than presenting identical text
   as two indistinguishable saves. The extraction inspector links to its original
   input and identifies the model. Neither record is hidden or deleted.
   Browse uses a single-column list; the inspector occupies space only after
   selection and can be closed without navigation. On narrow screens the
   selected inspector appears above the list and receives keyboard focus.
   To delete a memory, select it in **Browse**, then choose **Delete memory**
   in the inspector and confirm permanent deletion. This deletes the selected
   record, not all related memories. To retain a historical record instead,
   open **Read evidence & version history** and choose **Invalidate**.
   Search leads with a natural-language query; scope, historical filters and
   scoring controls remain available under **Advanced search options**.
   Retrieved memory text appears before collapsed retrieval diagnostics and
   ranking scores; no generated explanation is substituted for stored evidence.
   Empty search results and context sections serialize as empty arrays, not
   `null`. The UI also accepts legacy `null` collections and displays the
   no-matches state without JavaScript errors.
   Entity graphs, memory history and workspace lists also accept legacy empty
   collections. Entity neighborhoods return empty arrays for missing neighbors
   and current readable memories.
   After **Save now** confirms the original, controls are released immediately;
   extraction-status and embedding-coverage checks do not block another save
   through **Start over**. Failed writes retain their exact input and retry key.
   Action rows wrap within their panels, and wide data tables scroll locally
   rather than widening the page.

   Identity includes owner/run scope, exact audience/grants, trust, epistemic type,
   polarity, entity types/attributes and expiry. Casing/whitespace and terminal
   punctuation are normalized; substantive words outside the triple retain the
   whole quote as a conservative qualifier. Similarity or embeddings alone never
   establish equivalence, and paraphrases with different predicates may remain
   separate. Explicit world-validity starts are kept distinct. Ordinary
   current-time observations can reinforce one another regardless of worker order;
   the first committed interpretation retains its original validity/recording
   times. Reinforcement does not backdate an assertion.

   The matching-set absence check is enforced inside `store.Write(Batch)` on all
   three primary stores. Concurrent workers cannot both insert the same open
   identity; the losing worker retries its durable extraction snapshot. Evidence,
   relationships, jobs and result are committed together with stable operation keys.
   Closed/deleted assertions are never resurrected or mutated for reuse. Existing
   records are not retrospectively merged or reclassified. Identity lookup uses
   tenant-scoped indexed metadata filtering, not the small model candidate window.

   #### Per-tenant extraction profile and generated JSON Schema

   The default entity vocabulary is `person`, `organization`, `team`, `project`,
   `service`, `technology`, `database`, `location`, `document`, `concept`, `event`
   and `other`. All six epistemic claim types are enabled. Entities are extracted
   using model-provided `subject_type`/`object_type` annotations on claims, avoiding
   repeating the same evidence in the model output. Additional entities and optional
   attributes use the `entities` array; configured properties can also be emitted
   as `subject_attributes`/`object_attributes` directly on a claim. Both normalize to grounded typed entities in
   the public result. An empty inline type denotes a non-named value. Common
   English/Portuguese pronouns are excluded from entity indexes, with their original
   model classification explained in `normalization_notes` and the UI. No entity
   class is fabricated by normalization.
   Entities can also be extracted independently of triples, including entity-only inputs. An entity not attached
   to a claim receives an explicitly unreviewed `mentioned_as` observation, so it
   is indexed and linked to its evidence without inventing a factual relationship.
   Typed references are indexed; original type, quote and attribute annotations
   also remain on the immutable interpretation and extraction receipt.

   Tenant admins can open **Models & schemas -> Extraction schema**. The UI displays the actual
   generated provider schema and duplicate reinforcement outcomes. Changes preserve
   other tenant settings. Enable **Use server extraction defaults** to remove the
   override. The equivalent API is `GET /v1/settings`, then modify
   `knowledge.extraction` and `PUT /v1/settings` with the complete existing settings:

   ```json
   {
     "entity_types": ["person", "organization", "project", "service", "technology", "database"],
     "fact_types": ["fact", "observation", "belief", "hypothesis", "decision", "preference"],
     "entity_attributes": {
       "role": {"type": "string", "description": "Explicit role in the quoted text"},
       "environment": {"type": "string", "enum": ["production", "staging", "development"]}
     }
   }
   ```

   Server defaults can be configured using the same profile at
   `knowledge.extraction` in the CLI config, or `KnowledgeOptions.Extraction` in Go.
   Omitted/null overrides inherit the server profile. Entity types must be 1..24
   unique lowercase identifiers; claim types select 1..6 supported types. At most
   eight optional entity attributes are allowed: `string`, `number` or `boolean`
   (bounded string enums supported). Nested objects, arbitrary output structures,
   unknown attributes and unsupported types are rejected. Attribute values and
   entity classes are unreviewed model interpretations grounded by a source quote,
   not independent deterministic verification of each inferred property.

   The connector sends the generated JSON Schema to Ollama; OpenAI-compatible
   providers keep `json_object` mode and receive that exact contract in their
   instructions. Both paths use the same deterministic output validation.
   Safety, quote grounding, sizes, evidence, reference indexes, owner/audience and
   authority rules cannot be disabled by a tenant profile. Each plan snapshots its
   profile/schema, so changing settings cannot reinterpret an already planned
   retry. Disabled/shadow/enforced behavior is unchanged.

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

Each model receives one separately reported warm-up and thirty measured requests
by default (six cases, five repetitions). Cases cover a fact, a decision, a
greeting, an explicit conflict with a structured existing assertion, a repeated
statement with punctuation differences and Portuguese people/organization names.
Use `-repeats 2` before model names for a twelve-request check.
The conflict score includes deterministic polarity matching;
it is not a measure of model-only relationship inference. Output includes
every response/error, schema/grounding validation count, simple case-pass count,
expected named-entity presence checks and nearest-rank p50/p95. Entity presence
checks do not measure exhaustive entity recall or classification accuracy.
These small synthetic samples are **not** production
accuracy or latency guarantees. Inspect actual claims; passing grounding checks
does not establish semantic truth. A fast model returning empty arrays is not
counted as successfully extracting a substantive claim.

To check the real HTTP save/read-back path with a local Ollama model:

```sh
NIROMEM_TEST_LOCAL_EXTRACTION=1 go test -v ./connectors/knowledge \
  -run '^TestLocalModelExtractionThroughAPI$' -count=1 -timeout 15m
```

This opt-in test defaults to `LiquidAI/lfm2.5-350m`; override it with
`NIROMEM_TEST_EXTRACTION_MODEL`. It uses generated English and Portuguese
fixtures, an isolated in-memory store and a temporary HTTP server, never the
running application's database. It calls the real enrichment handler in
extractive mode, requires exact source excerpts, and checks that no structured
claims, entity annotations or graph links were invented. No UI tests are involved.

The [text-only configuration](../configs/lfm2.5-350m.example.json) retains its
historical filename but now uses **Qwen3.5:2B structured extraction** with
`think:false`, `extractive_only:false`, `num_ctx:8192` and `num_predict:1536`.
The provider deadline is 21 minutes and knowledge deadline 20 minutes for slow
CPU inference. EmbeddingGemma remains separate. Grounding validation is not
relaxed: invalid records are excluded with explicit issues, not invented
assertions or an excerpt fallback. Valid sibling records remain usable.
Tenant overrides can still narrow/replace the effective planner configuration.

The opt-in `TestLocalQwenStructuredExtraction` checks real Qwen on generated
English, Portuguese and Spanish statements. The end-to-end revision test uses
the example models, a generated statement, temporary SQLite and the native API:

```sh
NIROMEM_TEST_LOCAL_EXTRACTION=1 NIROMEM_TEST_LOCAL_EMBEDDING=1 \
  go test -v ./cmd/niromem -run '^TestLocalDocumentRevisionThroughAPI$' \
  -count=1 -timeout 9m
```

It verifies a committed document revision, a grounded structured item, persisted
768-dimensional vectors and source links. These tiny fixtures demonstrate the
working path, not production extraction accuracy or exhaustive document recall.
No application database or UI tests are involved. For larger sources, use
[manual document revisions](./documents.md#manual-incremental-revisions) to
recompute selected layers while retaining prior knowledge until atomic activation.

Other providers/configurations may still opt into `extractive_only:true`.
This mode works with Ollama and OpenAI-compatible endpoints without model-name
special cases; it remains distinct from structured extraction.
Provider `timeout_ms:0`
or omission retains the 11-minute default; its allowed range is 0..1860000.
Worker deadlines remain longer than the configured inference budget. These are
maximum waits, not artificial delays.

### Structured recovery and document coverage

The structured prompt includes multilingual procedural rules, requirements,
exclusions and thresholds, not just simple personal/project statements.
Structured assertions are checked for numeric values omitted from their selected
source quotes. A missing threshold, timeout or version triggers one focused
correction naming the lost values, not another whole-document pass. Persistent
loss is flagged as a window issue while other windows continue. This conservative
check can also flag omitted numeric references; it does not verify predicates,
comparison direction, units or general semantic completeness.

Passages containing multiple evidence spans are processed in independent
single-span windows, up to sixteen windows per passage. Each request remains bounded
to four facts/twelve entity annotations; the resolved aggregate permits 64 facts
and 320 entity annotations (including inferred inline annotations), with at most
eight candidate links. Unprocessed overflow is explicitly reported as a coverage
issue, never represented as complete extraction.

Each fact, entity and relation is validated independently. Invalid optional
entity typing/attributes can be excluded while retaining the grounded claim.
Invalid entities do not erase valid facts; links to rejected claims are excluded
and indexes are remapped. Every excluded record has an `issues` diagnostic.
Only when an entire unit is unusable is a bounded validation-feedback retry
attempted. Transport and truncated-output recovery retain their separate
classification. Splitting, record isolation and durable targeted repair are the
primary recovery mechanisms—not blind repetitions of a whole document.

Background results distinguish `partial` (grounded output with review gaps),
`empty` (model returned no structured records), `needs_review` and transport
failure. Empty output does not establish that the source lacks knowledge.
Document revisions can activate validated partial knowledge, expose the gaps,
and queue one targeted repair round; see [document revisions](./documents.md).

The opt-in `TestLocalQwenProceduralDocumentExtraction` checks generated Spanish
passages with multiple exclusions, installation requirements, a session timeout,
and signal/card thresholds. It asserts key details in the structured assertions,
not just nonempty JSON. It is still a small sample, not a document-recall guarantee.

### Optional excerpt-only extraction

In excerpt-only mode, the server segments source text into numbered verbatim spans of at most 2048
UTF-8 bytes. The model makes **one request** returning only `{"ids":[0,1]}`:
up to four distinct source IDs, or `{"ids":[]}` when none are useful. No candidate
memories, classification examples, entity vocabularies or relationship tasks are
sent. Ollama receives a bounded JSON schema; compatible endpoints receive the
same contract as instructions. The server validates the response and resolves
IDs to exact source text in `extraction.excerpts`. Repeated identical source
text is deduplicated. It never trusts model-written excerpts.

For document source chunks, extractive mode now records **`source_only`** with
an explicit reason, without invoking the selection model or creating duplicate
memories. Section-aware passages and their headings already provide searchable
source context. Deterministic document-section references are not inferred
claims or a semantic entity graph. Structured/staged planners and non-document
sources retain their existing contracts.

For non-document sources, in enforced mode selected excerpts become ordinary text memories, through the
existing ACL-preserving, deduplicating reconciler. Their metadata records source
ID/version, source text/offsets and model provenance. They have no generated
subject/predicate/object assertion, entity types, decision classification or
graph relationships. Existing normal memory preparation still normalizes stored
text; the extraction receipt retains the exact source slices. Matching existing
text is reused rather than duplicated. Excerpt writes enqueue normal embeddings
but do not recursively enqueue enrichment. In shadow mode excerpts are recorded
without applying memory changes.

Malformed/truncated model output, missing/null arrays, unknown fields, duplicate
IDs and invalid references produce a durable **`needs_review`** receipt with an
explicit error and warning log, not `completed` and not a dead job. No excerpts
are committed from rejected output and no automatic model repair loop runs.
The source version is checked before recording review. Retry of that queue job
reuses the receipt instead of calling the model again. Transport errors and
timeouts remain real errors with the existing durable retry behavior; they are
not disguised as model review. Existing dead jobs/receipts are not automatically
cleared or retried. Original storage and embedding jobs remain independent.

This guarantees that accepted excerpts come from source text, **not** that every
model selects the right sentences or that source statements are true. An empty
selection is not a completeness guarantee. Local LFM2.5-350m checks selected the
three generated English/Portuguese samples verbatim, but are not a production
accuracy certification. Existing source-size and deadline limits still apply.

### Legacy structured extraction

`staged_extraction: true` remains available for structured-extraction experiments.
It is mutually exclusive with `extractive_only`. Without either flag, the
original structured single-call contract remains in effect.

Staged extraction separates source-only claims, per-claim classification,
source-only entities, and optional candidate relationships. Only the relationship
stage receives candidate memories. Classification and entities cannot rewrite
the extracted triples. When a claim omits an evidence ID, the server selects the
first source span containing its subject, predicate and object; no matching
source span is an error. Entities still select explicit evidence IDs.
Stages use JSON generation with strict server-side field, type, evidence and
reference validation, rather than one large constrained generation grammar.
There are normally three calls for one claim without candidates, four with
candidates; additional claims each require their own classification call.
Empty claims skip classification and relationships, but still permit entity
extraction. Each invalid or truncated stage gets at most one repair attempt;
successful stages are not rerun. Repairs are recorded in extraction notes.
Transport failures are not hidden by repairs, and rejected annotations never
become a successful partial result. Only the complete normalized result can
reach the enrichment commit; the original source remains intact on failure.
All stages share the job's existing deadline and remain request-local/stateless.

Local verification of `LiquidAI/lfm2.5-350m` failed the original full-contract
checks. Staged trials improved the three sample claim triples, but classification
and entity output still failed live checks. JSON contracts and source evidence
references do not make this model's semantic extraction reliable. The example
structured modes are not an accuracy certification; inspect outputs before using
extracted assertions. `knowledge.mode: "shadow"` is available when evaluating
an unverified model without committing its annotations.

Local exploratory runs tested `gemma3:270m`, `smollm2:360m`, `qwen2.5:0.5b`,
`qwen2.5:1.5b`, `qwen3:0.6b` and `qwen3:1.7b`. The smaller Gemma/SmolLM and
Qwen2.5 trials had frequent grounding/format failures; the larger Qwen trials
were slower. Prompt/schema iterations were part of these trials, so they are
not a controlled leaderboard.

The original pre-typed-profile four-thread `qwen3:0.6b` pipeline, measured without concurrent test
execution on the local Core Ultra 7 165U CPU, produced:

| Warm requests | Valid/grounded outputs | Synthetic case checks | p50 | p95 |
|---|---|---|---|---|
| 20 | 20/20 | 20/20 | 2395 ms | 3607 ms |

With the typed profile, inline entity annotations and expanded six-case harness,
a separate twelve-request run (`-repeats 2`) passed **12/12** schema/grounding,
claim checks and expected named-entity presence checks: **3293 ms p50 /
4403 ms p95**. Source text is sent separately from optional candidate context to
reduce candidate copying by small models. This includes deterministic candidate-echo exclusion and polarity
matching; it is not model-only accuracy. A separate local request verified a
configured `role: engineer` attribute on the explicitly named person, not a
fabricated default attribute.

Separately, twenty new `/v1/facts` HTTP writes against the running SQLite server
with background processing enabled measured **6.2 ms p50 / 8.2 ms p95** on
localhost. Live read-back verified named entities, an explicit negative assertion
with a potential-conflict edge, and a proposed decision retaining the original
model classification. All synthetic verification memories were then removed
from their separate test tenant.

The original extraction samples are five repetitions of four small cases, not twenty independent knowledge
domains. The expanded harness adds two cases, not a production evaluation suite.
A simultaneous race-test workload caused timeouts in a separate run:
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

The console has three primary screens. HTMX loads authenticated, server-rendered
workspace fragments; existing JavaScript inspector modules keep using the real
JSON APIs. HTMX 2.0.8 and Pico CSS 2.1.1 are served locally with their licenses:
there are no CDN requests, inline scripts, or relaxed CSP requirements. Unknown
routes are rejected before importing modules; old bookmarks redirect to the
appropriate screen and preserve focused memory/turn identifiers.

| Screen | Real backend data and actions |
|---|---|
| Memories (`#/workspace`) | Browse cards, Remember, Search and Timeline. More tools includes knowledge state, exact transition receipts, entity graph and memory details. History and relationships stay connected to each memory. |
| Models & schemas (`#/models`) | Embeddings: actual active query model, dimensions, stored-vector coverage, durable backfill and embedding-job errors. Extraction schema: visible effective profile, editable entity types/attributes and the exact generated JSON Schema. Tenant settings remain available. |
| Operations (`#/operations`) | Jobs, metrics and read-only SQL. More tools retains rules, conversations and low-level ingestion/browsing. |

Use **Models & schemas -> Embeddings -> Embed existing memories** to enqueue
missing vectors for the configured embedder. Coverage refreshes while work is
outstanding; failed enqueue, provider, authorization and coverage requests remain
explicit. The worker must be running (the supplied Qwen config enables it inline).
Backfill does not change assertions or historical evidence. The active query
model includes the implicit server embedder, not just tenant model overrides.

The basic command without `-config` does not configure Ollama or an embedder.
For local model extraction and semantic search, start with
`-config configs/qwen3.example.json`. A tenant's saved `enforced` mode cannot
create a provider that was not configured at startup.

**Include older versions** also reveals expired memories within the loaded
slice. Timeline requests include expired history. Neither action restores,
rewrites or deletes records.

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
