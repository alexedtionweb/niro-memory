# Research report: agent memory systems

This report informed the architecture of niro-agent-memory. It answers four
questions: what to adopt, what to avoid, what to improve, and what makes this
engine different.

## Sources and confidence

| Section | How it was researched |
|---|---|
| Mem0 | **Read from source**: `mem0ai/mem0` at commit `26cdefb` (2026-10-26): `mem0/memory/main.py`, `mem0/utils/scoring.py`, `mem0/utils/entity_extraction.py`, `mem0/configs/prompts.py`, `mem0/memory/storage.py`. |
| TencentDB-Agent-Memory | **Read from source**: `TencentCloud/TencentDB-Agent-Memory` (README, `MemoryCore/src/core/{persona,scene}`, `MemoryPanel`). |
| Hindsight | **Read from the project README** (`vectorize-io/hindsight`). |
| Zep/Graphiti, Letta, LangGraph, Cognee, GraphRAG, A-MEM, hybrid IR, vector stores, memory poisoning | Compiled by a research sub-agent from primary papers and documentation it already knew; it could not re-fetch them in that session. Numbers are deliberately left out. Verify against the linked sources before citing them. |

No benchmark number from any vendor is used to justify a design decision.
Our own measurements are in [benchmarks.md](benchmarks.md).

---

## Mem0 (source-verified)

Paper: Chhikara et al., arXiv:2504.19413. The paper describes a two-stage
design: an LLM extracts facts, then an LLM decides ADD/UPDATE/DELETE/NOOP for
each one. **The current open-source code (v3 pipeline) works differently:**

- **Add** (`Memory._add_to_vector_store`):
  1. Load the last 10 messages of the session from a local SQLite `messages` table.
  2. Embed the new messages and run a vector search for the top 10 existing memories.
  3. Replace those memories' UUIDs with `"0"`, `"1"`, … so the LLM can't invent IDs.
  4. Make **one LLM call** with the "additive extraction" prompt. The operation is ADD-only. The prompt includes the observation date versus the current date, so relative times are grounded, plus a profile summary, custom instructions, and includes/excludes. The answer is JSON.
  5. Embed all extracted texts in one batch, falling back to one-by-one.
  6. Drop duplicates by MD5 of the raw text, both within the batch and against existing memories.
  7. Insert in a batch and write rows to a local `history` table.
  8. Run spaCy entity extraction (proper nouns, quoted text, noun compounds, identifiers) and write an **entity vector store** whose payloads hold `linked_memory_ids`. An entity merges with an existing one on exact normalized text or cosine ≥ 0.95.
  - LLM errors are raised to the caller, not swallowed.
  - With `infer=False`, each message is stored verbatim.
- **Search** (`_search_vector_store`):
  - The query is lemmatized for BM25, spaCy extracts query entities, and the query is embedded.
  - Vector search over-fetches `max(4·limit, 60)` candidates; keyword search runs when the vector store supports it.
  - BM25 scores are squashed through a **sigmoid whose midpoint and steepness depend on query length**.
  - The **entity boost** is `similarity × 0.5 × 1/(1+0.001·(n−1)²)`, where `n` is how many memories link to the entity.
  - The final score adds semantic + BM25 + entity and divides by the maximum possible from the *active* signals, giving a [0,1] score.
  - The semantic score must pass the threshold before any boosting. With `explain`, the score components are returned.
- **Filters**: eq/ne/gt/gte/lt/lte/in/nin/contains/icontains, AND/OR/NOT, and a `*` wildcard. Scopes are `user_id`/`agent_id`/`run_id`; `expiration_date` and `show_expired` are supported.
- **Procedural memory** is an LLM summary of an agent's execution history.

**Weaknesses observed in the code:**
1. Contradicting facts coexist because extraction is ADD-only.
2. Every search embeds the query, so the embedding is always on the critical path.
3. Candidates come only from vector results, so BM25-only hits are lost.
4. `add()` makes a synchronous LLM call.
5. The node is stateful (local SQLite history file, spaCy model held in process).
6. `linked_memory_ids` is updated by read-modify-write on a payload array, which can race under concurrent writers.

## TencentDB-Agent-Memory (source-verified)

- **Layers:**
  - L0 conversation
  - L1 atoms (facts, preferences, constraints, events)
  - L2 scenes (knowledge blocks per project or topic, with "heat" = hit count)
  - L3 persona
- **Retrieval** starts with L3 and a scene navigation index (progressive disclosure), then falls back to BM25 + vector + RRF over L1/L0. Results are capped by item count, characters, and timeout.
- **Persona regeneration is gated:**
  - It triggers on an explicit request, on accumulated new atoms, or on changed scenes.
  - An LLM labels the change with strict priority `conflict > boundary_change > new_rule > reinforcement > none`.
  - Only the first three cause a rewrite.
- **Memory assets** (chat memory, skills, wiki, code graph) have visibility private / team / restricted with user, role, and agent ACLs, and are bound to agents.
- **Skills** are versioned, with triggers, steps, validation rules, and a review → share flow.
- **Weaknesses:**
  - Scenes, personas, and checkpoints are files, which makes the node stateful.
  - The system is three Node services.
  - LLM calls are needed for every layer above L0.

## Hindsight (README-verified)

- Memory types: world facts, experiences, observations (consolidated beliefs with evidence quotes and a proof count), and mental models (standing answers that are rewritten in the background and read without any LLM call).
- `retain` uses an LLM to extract facts, time, entities, and relationships.
- `recall` runs four strategies **in parallel**: semantic, BM25, graph (entity/temporal/causal), and temporal range. It merges them with RRF, reranks with a cross-encoder, and trims to a token limit.
- `reflect` is LLM reasoning over memories.
- Memories are partitioned into "banks"; the backing store is PostgreSQL.
- **Lessons:**
  - Evidence-backed consolidated beliefs are worth having.
  - "Read a precomputed answer with no LLM" matches our layered `Context()`.
  - Running every strategy on every query is what our planner avoids.

---

## 1. Zep / Graphiti

**Paper:** Rasmussen et al., "Zep: A Temporal Knowledge Graph Architecture for Agent Memory," arXiv:2501.13956 (https://arxiv.org/abs/2501.13956). **Engine:** Graphiti, open source, https://github.com/getzep/graphiti.

**Architecture/data model:** A property graph of three node types — episodic nodes (raw ingested messages/text/JSON), entity nodes, and community nodes — connected by typed edges representing facts. Backed by Neo4j or FalkorDB.

**Ingestion:** Episodes are added incrementally (no full reprocessing/batch recompute), which the paper positions against GraphRAG-style static batch indexing.

**Extraction/consolidation:** Each episode triggers several LLM calls: entity extraction, edge/fact extraction, deduplication against semantically similar existing nodes/edges (found via embedding search), and temporal-attribute extraction to assign validity timestamps. The paper explicitly tries to keep per-write cost down by using smaller/faster models for some sub-steps — so writes are *not* LLM-free, but the team optimizes call count and model size.

**Temporal model (bi-temporal):** Edges carry four timestamps — `created_at`/`expired_at` (transaction time: when the system learned/retracted a fact) and `valid_at`/`invalid_at` (event time: when the fact was true in the world). When a new edge contradicts an existing one, the old edge is **invalidated** (its `invalid_at` is set) rather than deleted, preserving full history for point-in-time queries.

**Entity resolution:** Candidate duplicate nodes are found via embedding similarity, then merged/confirmed with LLM judgment.

**Retrieval:** Hybrid search combining BM25 full-text, cosine similarity over node/edge embeddings, and graph BFS traversal from seed nodes, fused and reranked (reciprocal rank fusion / MMR) with an optional cross-encoder reranker. Retrieval itself is **not** LLM-dependent — it's DB queries + embeddings + a reranker model; only final answer generation calls the LLM, which the paper argues is why Zep achieves lower query latency than agent-orchestrated memory retrieval.

**Reported latency/scale:** The paper reports accuracy gains over MemGPT on the Deep Memory Retrieval (DMR) benchmark and on LongMemEval, plus large latency reductions versus stuffing full conversation history into context. Treat the exact percentages as self-reported by the authors; see the dispute note in §9 regarding cross-vendor comparisons.

**Weaknesses:** Multiple LLM calls per write add cost/latency and extraction-error risk; graph BFS cost grows with graph size; requires a graph DB operational dependency (Neo4j/FalkorDB).

## 2. Letta / MemGPT

**Paper:** Packer et al., "MemGPT: Towards LLMs as Operating Systems," arXiv:2310.08560 (https://arxiv.org/abs/2310.08560). **Project:** Letta (rebrand of MemGPT), https://github.com/letta-ai/letta, https://www.letta.com/.

**Architecture/data model:** OS-inspired virtual-context management with three tiers: **core memory** (in-context, token-budgeted editable blocks, e.g., persona/human), **archival memory** (out-of-context, vector-indexed store for large facts), and **recall memory** (full interaction history, DB-searchable).

**Ingestion/extraction:** No separate consolidation pipeline — the agent LLM itself issues function calls (`core_memory_append`, `core_memory_replace`, `archival_memory_insert`, `archival_memory_search`, `conversation_search`) inline during its normal turn. Memory writes are agent-driven tool calls rather than a dedicated extraction step, so "LLM calls per write" is effectively folded into ordinary reasoning turns.

**Retrieval:** Archival search uses vector similarity; recall memory search is DB/keyword/date-based. Critically, retrieval is **LLM-orchestrated** — the agent must decide to invoke a search tool, so the loop requires an LLM in order to retrieve at all (unlike Zep's mechanical hybrid search).

**Letta sleep-time agents:** A background agent runs during idle periods to reorganize/consolidate memory blocks without blocking the main conversational agent (documented on Letta's blog/docs, https://www.letta.com/).

**Temporal model/entity resolution/contradiction handling:** No native bi-temporal schema; timestamps order recall-memory messages. Contradictions in core memory are resolved implicitly whenever the LLM chooses to replace text — there's no automatic conflict detector.

**Weaknesses:** Memory quality depends entirely on the agent's own judgment about when to read/write; no structured entity graph; the DMR benchmark used for comparison originates from this same paper, which later work (Zep) uses as a baseline — worth noting when comparing numbers across papers.

## 3. LangGraph Memory (+ LangMem)

**Docs:** https://langchain-ai.github.io/langgraph/concepts/persistence/ and https://langchain-ai.github.io/langgraph/concepts/memory/. **LangMem:** https://github.com/langchain-ai/langmem.

**Architecture:** LangGraph separates two concerns. **Checkpointers** (`MemorySaver`, `SqliteSaver`, `PostgresSaver`) persist full graph state at every super-step — this is *thread-scoped short-term memory* enabling resumption, time-travel, and human-in-the-loop interrupts. **`BaseStore`** is a cross-thread, namespaced key-value interface (namespaces are tuples, e.g., `(user_id, "memories")`) supporting `put`/`get`/`search`, with an optional embedding index for semantic search over stored items.

**LangMem:** A higher-level library on top of `BaseStore` offering three memory "types": **semantic** (facts, as either an evolving profile object or a growing fact collection), **episodic** (few-shot examples of past successful trajectories), and **procedural** (self-updating system-prompt/instructions based on reflection). It provides `create_memory_store_manager` for background extraction/consolidation (this step uses an LLM to decide what to extract or merge) and agent-callable tools (`create_manage_memory_tool`, `create_search_memory_tool`) for in-the-loop memory management.

**Retrieval/LLM need:** `BaseStore.search` can do pure vector similarity without an LLM call; LangMem's manager-driven consolidation requires an LLM to decide extraction/merges.

**Weaknesses:** `BaseStore` is intentionally a thin KV abstraction — no native graph relationships, bi-temporal invalidation, or built-in BM25+vector fusion; all of that must be layered on by the developer. The "profile" semantic-memory pattern uses whole-document patch updates that can get costly as profiles grow.

## 4. Cognee

**Repo/docs:** https://github.com/topoteretes/cognee, https://docs.cognee.ai/.

**Architecture:** The "ECL" pipeline (Extract, Cognify, Load — echoing ETL) runs modular tasks as DAG pipelines: chunking → embedding → LLM-driven entity/relationship extraction (optionally constrained by a user-supplied ontology) → graph construction → load into a combined store: a graph DB (NetworkX in-memory by default, or Neo4j/Kuzu) plus a vector DB (LanceDB default, or Qdrant/pgvector/Weaviate) plus relational metadata (SQLite/Postgres).

**Retrieval:** A `search` API supports multiple retrieval modes (summaries, insights, raw chunks, "graph completion"); some modes need an LLM to synthesize an answer from retrieved graph context, while raw node/edge retrieval does not strictly require one.

**Entity resolution/contradiction handling:** Relies on LLM extraction plus optional ontology constraints to reduce duplicate entity/relation types; unlike Graphiti, there is no first-class bi-temporal edge-invalidation model — Cognee is closer to a general-purpose GraphRAG-style knowledge builder than a purpose-built evolving conversational memory.

**Weaknesses:** Young, fast-moving API; extraction cost scales with LLM calls per chunk similarly to GraphRAG; versioning/temporal support less mature than Graphiti or purpose-built agent-memory systems.

## 6. Microsoft GraphRAG

**Paper:** Edge et al., "From Local to Global: A Graph RAG Approach to Query-Focused Summarization," arXiv:2404.16130 (https://arxiv.org/abs/2404.16130). **Repo/docs:** https://github.com/microsoft/graphrag, https://microsoft.github.io/graphrag/.

**Architecture/ingestion:** Builds an entity knowledge graph from source documents via per-chunk LLM entity/relationship extraction, with repeated "gleaning" passes to recover missed entities (each gleaning pass is an additional LLM call). The entity graph is then clustered hierarchically using the **Leiden algorithm** into nested communities, and each community gets an LLM-generated summary, bottom-up through the hierarchy.

**Retrieval:** **Global search** answers broad, corpus-wide questions via map-reduce over community summaries at a chosen hierarchy level (many parallel LLM calls in the map step, one reduce call). **Local search** answers entity-focused questions: vector-match the query to entities, expand to their relationships/text units/claims within a token budget, then a single LLM synthesis call — notably cheaper than global search.

**Indexing cost:** This is the most-cited criticism of GraphRAG — indexing cost scales with (chunks × extraction/gleaning passes) plus community summarization at every hierarchy level. Microsoft's own project pages acknowledge this and introduced "LazyGraphRAG" as a cheaper variant that defers expensive summarization to query time rather than doing it eagerly at index time (documented on the GraphRAG project site/blog, https://microsoft.github.io/graphrag/).

**Weaknesses:** Expensive, largely batch-oriented indexing; originally designed for static corpora rather than continuously streaming conversational writes (incremental-update support was added later but is not the core design point); better suited to corpus-level summarization/QA than turn-by-turn agent memory.

## 7. A-MEM

**Paper:** Xu et al., "A-Mem: Agentic Memory for LLM Agents," arXiv:2502.12110 (https://arxiv.org/abs/2502.12110).

**Architecture/data model:** Zettelkasten-inspired. Each memory is a "note" containing content, timestamp, LLM-generated keywords/tags/contextual description, and explicit links to related notes.

**Extraction/consolidation (LLM calls per write):** At least one LLM call constructs the new note's description/keywords/tags. The system then retrieves the *k* most similar existing notes via embeddings and uses an LLM to decide whether to create links and whether to **update those neighboring notes' metadata** (not raw content) in light of the new note — i.e., a single insert can cascade into several additional LLM calls against older memories, approximating human associative memory consolidation ("dynamic memory evolution").

**Retrieval:** Embedding similarity search over notes/descriptions finds candidates; the similarity step itself doesn't need an LLM, but note construction and link-maintenance at write time do.

**Weaknesses:** Write-time cost scales with the number of neighbor updates triggered per insert; link structure quality depends on LLM judgment and is less deterministic/auditable than rule-based schemas; reported benchmark numbers in the paper are self-evaluated and should be read as such rather than independently confirmed here.

## 9. Hybrid Retrieval: BM25 + Dense Fusion, RRF, MMR

BM25 (sparse, lexical, strong on exact terms/out-of-vocabulary tokens) and dense embedding cosine similarity (semantic/paraphrase matches) are complementary; fusing their ranked lists typically improves recall over either alone — this is the retrieval pattern used by Graphiti, Cognee, and most production RAG/memory stacks.

**Reciprocal Rank Fusion (RRF):** Cormack, Clarke & Buettcher, "Reciprocal Rank Fusion outperforms Condorcet and Individual Rank Learning Methods," SIGIR 2009 (https://dl.acm.org/doi/10.1145/1571941.1572114). RRF combines ranked lists *without* needing to normalize incomparable scores: `score(d) = Σ_rankers 1/(k + rank(d))`, with `k=60` the constant the original paper found empirically robust across TREC collections — this is why `k=60` shows up as a near-universal default in hybrid-search implementations.

**Score normalization alternative:** min-max or z-score normalization followed by a weighted sum is more tunable but more fragile — thresholds and weights are corpus- and query-distribution-dependent, whereas RRF only needs rank position.

**MMR:** Carbonell & Goldstein, "The Use of MMR, Diversity-Based Reranking for Reordering Documents and Producing Summaries," SIGIR 1998 (https://dl.acm.org/doi/10.1145/290941.291025). MMR reranks a candidate set to trade off relevance against redundancy: `argmax_d [λ·Sim1(d,q) − (1−λ)·max_j Sim2(d,d_j)]` over already-selected `d_j` — used after fusion to keep near-duplicate memories from crowding out distinct relevant ones.

## 10. Vector Stores for a Go System

- **Brute-force flat scan:** Simplest, zero external dependency, exact recall, trivially embeddable in pure Go. Scales fine up to roughly tens of thousands to low millions of vectors depending on dimensionality/hardware, after which linear scan latency becomes the bottleneck.
- **sqlite-vec (`vec0`):** https://github.com/asg017/sqlite-vec — the successor to sqlite-vss, embeddable via SQLite virtual tables. As documented in the project's README/roadmap, `vec0` currently performs **brute-force exact nearest-neighbor search** (no ANN graph/IVF index yet); partitioning/ANN support is tracked as a roadmap item rather than shipped. Good fit for single-binary embedded Go apps that don't need ANN-scale.
- **pgvector:** https://github.com/pgvector/pgvector — supports both **HNSW** (graph-based, higher recall/query speed, costlier/slower to build and update) and **IVFFlat** (needs a training/clustering step over existing data, faster to build and lower memory, recall tunable via `probes`). Release 0.8.0 added **iterative index scans**: when a query combines an ANN index with a restrictive `WHERE` filter, the index scan keeps pulling additional candidate batches (instead of stopping at `ef_search`/`probes` and under-returning) until enough post-filter rows are found — directly relevant to any Go system doing filtered hybrid retrieval over Postgres. Good fit when you already need transactional colocated metadata and SQL filtering alongside vectors.
- **Qdrant:** https://qdrant.tech/documentation/guides/multiple-partitions/ documents a payload index with an `is_tenant` flag: marking a payload field as the tenant key tells Qdrant to colocate that tenant's vectors on disk/in a segment, making filtered per-tenant search efficient without paying full per-tenant HNSW-graph cost. Strong fit for true multi-tenant isolation at scale.
- **Milvus:** https://milvus.io/docs — a distributed vector database separating compute/storage, offering multiple index families (exact `FLAT`, approximate `IVF_FLAT`/`IVF_PQ`, `HNSW`, disk-based `DiskANN` for billion-scale corpora). Best fit for very large scale or when vector search needs to scale independently of the primary store, at the cost of extra operational components (coordinator services, object storage, message queue in distributed mode).

**Trade-off summary for a bring-your-own-vectors Go engine:** flat/sqlite-vec for small/embedded deployments where exactness matters more than scale; pgvector when the durable job queue / bi-temporal records already live in Postgres and transactional consistency with metadata matters; Qdrant/Milvus when scale or strict multi-tenant isolation outgrows a single relational instance.

## 11. Memory Poisoning / Prompt Injection via Agent Memory

- **MINJA** (arXiv:2503.03704, https://arxiv.org/abs/2503.03704): shows that an attacker with *only ordinary interaction access* (no direct database access) can poison an agent's long-term memory by crafting multi-turn conversations that get recorded as memory entries, using bridging/indication steps so the malicious record appears contextually relevant and is later retrieved for unrelated victim queries, steering the agent toward incorrect or harmful outputs.
- **AgentPoison** (arXiv:2407.12784, https://arxiv.org/abs/2407.12784): a red-teaming/backdoor attack that optimizes a trigger so triggered queries map, in embedding space, to a tight cluster near a small number of injected poisoned entries in the agent's memory or RAG knowledge base — causing retrieval of the attacker's malicious demonstrations while benign-query performance stays largely intact. Demonstrated across multiple agent types (e.g., planning, QA, healthcare agents) in the paper's experiments; only a small number of poisoned entries need to be injected.
- **Mitigations** converging across both papers and general practice: (1) treat retrieved memory content as **untrusted data, never as instructions** (strict prompt/data separation); (2) attach provenance/trust metadata (source, actor, confidence) to every memory record and let retrieval/policy downweight or exclude low-trust entries; (3) anomaly/outlier detection over the embedding space to catch the tight poisoned clusters AgentPoison produces; (4) write-path controls — rate limits, per-actor quotas, approval gates before memories become durable/retrievable, and schema validation/allow-listing (deterministic extraction rather than free-form LLM writes shrinks the natural-language payload attack surface); (5) periodic audit/replay of stored memories against their source conversations.
---

## Conclusions

### Adopt
- **Bi-temporal records** (Graphiti), applied to *every* memory: corrections and changes close old versions instead of deleting them.
- **Hybrid retrieval**: BM25 + vector + entity + graph, fused with Mem0's normalized additive score (thresholdable, explainable), with RRF (k=60) as an alternative and MMR for diversity.
- **Mem0's useful mechanics**: `infer=false`, hash dedup, per-memory history, the entity-boost popularity damping, query-length-adaptive BM25 normalization, the filter language, and the user/agent/run scopes.
- **TDAM layering**: L0–L3, progressive disclosure through a scene index, budgets on assembled context, visibility levels with grants, and agent bindings.
- **LangGraph's split** between execution state and long-term memory. We hold run state (cursor, summary) apart from memories.
- **Provenance and trust as schema**, against MINJA/AgentPoison-style poisoning.

### Avoid
- An LLM on the write path or the read path of the core. Every system reviewed puts at least one LLM call per write (Mem0, Graphiti, A-MEM, Cognee, Hindsight, GraphRAG). Letta needs an LLM even to *retrieve*.
- Running every retrieval strategy on every query (Mem0 always embeds; Hindsight always runs four).
- Process-local state that correctness depends on: Mem0's local SQLite history and spaCy, TDAM's files and checkpoints.
- GraphRAG-style batch community summarization for streaming conversational data.
- A mandatory graph database (Graphiti needs Neo4j/FalkorDB). The co-occurrence graph and relationship memories work on any SQL store.

### Improve upon
- **Deterministic supersession.** A slot key ("user.location") makes "the user moved" a cheap, testable, replayable version change instead of an LLM verdict.
- **Explainable planner.** The planner chooses the cheapest sufficient strategies and reports what it ran, what it skipped, and why.
- **Connector contract** with conformance kits. Storage, vector index, blob store, cache, and embedder can each be swapped without touching the core.
- **Bring-your-own vectors** of any modality. The engine never needs to know your embedding model.
- **Stateless processing**: outbox enqueue, leased jobs, a run cursor with compare-and-set, and idempotent stage writes.

### Differentiators
1. LLM-free core with a sub-millisecond-class hot path. AI is an optional extension, never a dependency.
2. One bi-temporal record type for every kind and layer, with evidence pointing at exact byte spans of episodes.
3. A JSON rules file for deterministic extraction that tests itself, can be overridden per tenant, and is versioned for re-derivation.
4. A zero-dependency core module with explicit, niro-style connector wiring.
5. A dev console that shows planner traces, version timelines, and the rule pipeline live.
