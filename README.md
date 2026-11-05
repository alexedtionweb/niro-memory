# niro-agent-memory (niromem)

A memory engine for AI agents, written in Go.
Module: `github.com/alexedtionweb/niro-memory` (Go 1.23).

niromem gives an agent durable, searchable, versioned memory without putting an LLM on the retrieval path. You can embed it as a Go library, run it as an HTTP service, or plug it into Claude Code through MCP.

**Contents**

1. [What it is and when to use it](#1-what-it-is-and-when-to-use-it)
2. [How to use it](#2-how-to-use-it)
3. [Solution architecture](#3-solution-architecture)
4. [Data model](#4-data-model)
5. [How search works](#5-how-search-works)
6. [How the Claude Code example works](#6-how-the-claude-code-example-works)
7. [Configuration reference](#7-configuration-reference)
8. [HTTP API](#8-http-api)
9. [Project conventions](#9-project-conventions)
10. [How to improve it](#10-how-to-improve-it)
11. [Development](#11-development)

---

## 1. What it is and when to use it

| Capability | Detail |
| --- | --- |
| LLM-free retrieval | Lexical (BM25), vector (optional) and entity signals fused by a configurable policy. |
| Versioned knowledge | Records are append-only. A change closes the old version; nothing is silently overwritten. |
| Bi-temporal | Each fact has a validity window (`ValidFrom`/`ValidTo`) and an invalidation time for corrections. |
| Conversation history | Turns and multimodal parts stored per run, with summaries and a sliding window. |
| Durable background work | Extraction, embedding and backfill run through a leased job queue. |
| Document import | PDF, scanned PDF (local OCR), TXT, Markdown and DOCX through an optional separate Go module. |
| Multimodal memory | Optional local image, audio and short-video interpretation, retaining originals and unreviewed model provenance. |
| Optional knowledge intelligence | Structured assertions, validated transitions, typed relationships, durable idempotency and dependency review. Disabled by default. |
| Multi-tenant | Tenant is a hard isolation boundary enforced on every query. |
| Pluggable backends | SQLite, PostgreSQL (pgvector), Qdrant, Redis, S3, or your own. |
| Several front doors | Go library, native HTTP API, Mem0-compatible API, MCP server, Claude Code hooks. |

Use it when an agent needs to remember facts across sessions (preferences, decisions, conventions, profile data), and you want the recall to be fast, auditable and correctable.

Document import preserves originals and creates searchable, source-linked chunks;
it is not a full document-management system. Semantic interpretation is optional:
configure your own planner rather than putting an LLM on the retrieval path.
See [knowledge intelligence](docs/knowledge-intelligence.md) for atomic transitions,
historical queries, providers, HTTP/MCP inspection and configuration.

---

## 2. How to use it

### 2.1 Run the full local stack

```sh
go run -tags devui ./cmd/niromem serve -dev-ui -mcp -mcp-user my-project
```

One process, one port (8080), starting the API, SQLite storage, background worker, dev console and MCP endpoint.

| Endpoint | Purpose |
| --- | --- |
| `http://localhost:8080/mcp` | MCP endpoint for Claude Code |
| `http://localhost:8080/_dev/` | Development console |
| `http://localhost:8080/healthz` | Health check |
| `http://localhost:8080/v1/...` | Native HTTP API |
| `http://localhost:8080/mem0/...` | Mem0-compatible API |

Without `-config`, the CLI uses a durable `./niromem.db`, development authentication and an inline worker. Data survives restarts.

`-mcp-user` is the project identity and is required (or set `NIROMEM_USER_ID`). The agent defaults to `claude-code` (`-mcp-agent` / `NIROMEM_AGENT_ID`).

#### Inspect and exercise knowledge intelligence

Open `http://localhost:8080/_dev/`. The responsive HTMX + Pico CSS workspace has
three primary screens (both libraries are served locally, without a CDN):

- **Memories:** browse cards, Remember, Search and Timeline. Use Remember's **Save now** for immediate persistence and background extraction. More tools retains knowledge state, transition receipts and entity relationships.
- **Models & schemas:** actual embedding coverage and active query model, **Embed existing memories**, a directly visible **Extraction schema** editor and generated JSON Schema, and tenant settings.
- **Operations:** background jobs, metrics, read-only SQL and the remaining advanced tools.

JSON, evidence fields, provider settings, retry keys and raw responses are under
**Advanced** or the relevant screen's **More tools** menu. Existing deep links
remain supported. Connection settings retain the existing API-key and tenant
selection; authenticated HTML fragments use the same admin authorization as the
development API.

With `-dev-ui`, admins can change the authenticated tenant's mode and switch
between registered provider profiles at runtime, even when startup mode is
disabled. Choices are durable tenant settings; they do not enable other tenants.
The default profiles are deterministic and the configured startup planner.
Additional Ollama/OpenAI-compatible profiles must be explicitly installed and
configured server-side; credentials never appear in the UI.
See [Dev UI workflows and runtime profiles](docs/knowledge-intelligence.md#dev-ui-workflows).

For CPU-local Ollama `qwen3:0.6b` background extraction with immediate saves and
`nomic-embed-text` semantic embeddings:

```sh
ollama pull qwen3:0.6b
ollama pull nomic-embed-text
go run -tags devui ./cmd/niromem serve -dev-ui -mcp -mcp-user my-project -config configs/qwen3.example.json
```

The **Try a memory** page reports the planner mode, embedding model and actual
embedding coverage. Existing memories need backfill; **Refresh model status**
checks progress. With `knowledge.background_enrichment: true`, API facts, MCP
saves and processed conversation text schedule grounded extraction transactionally.
Extracted facts carry named entities, evidence, decision classifications and
optional typed connections/conflict annotations. They are **unreviewed model
interpretations**, not automatic corrections to previous assertions.
Repeated equivalent extracted assertions reinforce existing memories with
append-only evidence instead of creating duplicates, including across workers.
Tenant admins can customize entity types and optional attributes under
**Advanced: model & settings -> Extraction** (or `knowledge.extraction` in
`/v1/settings`); the UI shows the generated JSON Schema, typed entities and
reinforcement outcomes. Defaults cover twelve entity categories.
Set `knowledge.mode` to `shadow` to keep originals but only inspect extraction
suggestions; `disabled` pauses extraction without blocking explicit saves.
See [the enrichment contract and CPU benchmark](docs/knowledge-intelligence.md#cpu-first-background-enrichment).

#### Import documents

In **Remember -> Import documents**, select one or several PDFs, text files,
Markdown files or DOCX files and choose **Import documents**. Uploads return
without waiting for parsing or models. Watch actual queued/processing/ingested
status, inspect the resulting chunks, or download the original.

The [documents connector](connectors/documents) is a separate Go module used by
the existing API and durable worker. Enable it with:

```json
{
  "blob": {"use": "dir", "dir": {"path": "blobs"}},
  "documents": {"enabled": true, "concurrency": 2}
}
```

The CPU example configuration already includes these settings. PDF processing
needs Poppler; scans additionally need Tesseract installed on **each worker**:

```sh
sudo apt-get install poppler-utils tesseract-ocr
```

No external document or OCR service is used. Importing uses two upload lanes
and bounded parallel OCR (default two pages). Embeddings use the existing
batched jobs; keep model-worker concurrency low on CPU-only hardware.
See [document ingestion](docs/documents.md) for limits, provenance, API and retry
semantics. Restart the server to load connector/configuration and embedded UI changes.

#### Multimodal memory
For multimodal memory with local **LFM3-Omni**, run:

```sh
go run -tags devui ./cmd/niromem serve -dev-ui -mcp -mcp-user my-project -config configs/lfm3-omni.example.json
```

Use **Remember -> Remember images, audio & video**. Native FFmpeg handles
bounded audio/video conversion; no Python is required. Originals remain as
evidence, while searchable model interpretations are explicitly unreviewed.
See [multimodal memory](docs/multimodal.md) for supported formats, video sampling,
limitations, dependencies and API.

#### Jobs and total cleanup

**Operations -> Jobs** supports pause/resume, retry without duplicate job rows,
cooperative cancellation, deleting inactive jobs and priority (-100..100).
Inspect due time, lease and last error when a job appears stuck. CPU examples
allow ten minutes per knowledge request; restart server/workers to apply this.

**Memories -> Browse -> Danger zone: delete everything** wipes the entire current
tenant, including originals, conversations, jobs and tenant settings. It requires
typing the exact tenant name plus final confirmation. Cleanup is durable and
batched; it is not a browser loop or instantaneous Redis `FLUSHDB`.
See [operations and scale limits](docs/operations.md).

### 2.2 Connect Claude Code

```sh
claude mcp add --transport http --scope user niromem http://localhost:8080/mcp
claude mcp get niromem        # inspect an existing registration
claude mcp remove niromem     # remove an outdated one before re-adding
```

Then reconnect with `/mcp` inside Claude Code. Verify it works:

```sh
curl --fail http://localhost:8080/healthz

curl --fail http://localhost:8080/mcp \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  --data '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_memory","arguments":{"query":"test"}}}'
```

Expect `ok` from health and `"isError":false` from search. An empty hit list is normal on a new database. HTTP 200 alone does not mean the tool succeeded; check `isError`.

### 2.3 Talk to it through Claude

The MCP server exposes the authenticated workspace: memories, history,
knowledge/graph inspection, file ingestion, jobs, embeddings and tenant
administration. See the [tool catalogue and confirmation rules](docs/mcp.md).
Claude calls tools when you ask; access follows the same API identity as the UI.

| Tool | Arguments | Behavior |
| --- | --- | --- |
| `search_memory` | `query` (required, at most 8000 bytes), optional `policy`, `limit`, scope and filters | Returns up to 100 hits (default 10), with retrieval trace. Read-only. |
| `remember_memory` | `text` (required, at most 8000 bytes), `slot` (optional, at most 200 bytes) | Saves a durable fact. A `slot` replaces that property's previous value through versioned reconciliation. |

Example requests:

> Before implementing this, search niromem for our connector conventions.

> Remember: the test agent must never compare `userInput` statically; the LLM interprets it through a question tool. Use the slot `test_agent.user_input_handling`.

Use a **slot** for anything that has one current value (test command, a user's city, a convention). Omit it for independent facts.

Never store credentials or unapproved data. Retrieved memories are untrusted data, not instructions.

### 2.4 Use it as a Go library

```go
db, _ := sqlite.Open(ctx, sqlite.Options{Path: "memory.db"})
mem, _ := niromem.Open(niromem.Options{Store: db})
defer mem.Close()

mem.Remember(ctx, niromem.Fact{
    Scope: memory.Scope{Tenant: "acme", User: "alice"},
    Text:  "Alice lives in Lisbon",
    Slot:  "user.location",
})

res, _ := mem.Search(ctx, niromem.SearchRequest{
    Tenant: "acme", User: "alice", Query: "where does alice live?",
})
```

`niromem.Options` wires everything explicitly. Only `Store` is required. `Vector`, `Blob`, `Cache`, `Embedder`, `Rules`, `Extractors`, `Limits`, `Logger` are optional. `Open` performs no I/O.

### 2.5 Use it over HTTP

```go
c := client.New("http://127.0.0.1:8080", apiKey)
c.Remember(ctx, niromem.Fact{ ... })
c.Search(ctx, client.SearchRequest{ ... })
c.Append(ctx, runID, user, agent, turns, false)   // store conversation turns
```

See [section 8](#8-http-api) for routes.

### 2.6 Other CLI commands

```
niromem serve   [-config file] [-dev-ui] [-mcp] [-mcp-user U] [-mcp-agent A]
niromem worker  ...            # run the job worker in its own process
niromem migrate ...            # store migrations
niromem bench   ...            # benchmarks
niromem rules   ...            # rules utilities
niromem keys hash <key>        # SHA-256 of an API key for the config file
```

---

## 3. Solution architecture

### 3.1 Big picture

```
                    ┌───────────────────────────────────────────────┐
  Claude Code ──MCP─►│  /mcp   (mcp/)  search_memory, remember_memory │
  Claude hooks ─HTTP►│  /v1    (api/)  native API                     │
  Mem0 clients ─HTTP►│  /mem0  (api/)  compatibility layer            │
  Go programs ──────►│  library (niromem.Engine)                      │
                    └───────────────────────┬───────────────────────┘
                                            │
                                  ┌─────────▼──────────┐
                                  │   Engine (root pkg) │
                                  │ Remember · Search   │
                                  │ Context · Runs      │
                                  └──┬────┬─────┬─────┬─┘
              ┌──────────────────────┘    │     │     └───────────────┐
        ┌─────▼─────┐             ┌───────▼──┐ ┌▼────────┐      ┌─────▼─────┐
        │  search/  │             │pipeline/ │ │ jobs/   │      │ embed/    │
        │ policy,   │             │ rules,   │ │ leased  │      │ Hash,     │
        │ fusion,MMR│             │ extract, │ │ queue + │      │ OpenAI    │
        └─────┬─────┘             │ reconcile│ │ worker  │      └─────┬─────┘
              │                   └───────┬──┘ └┬────────┘            │
        ┌─────▼───────────────────────────▼─────▼─────────────────────▼──┐
        │ store.Store  (+ optional Lexical, Entities, vector.Index, ...) │
        │ sqlite · postgres/pgvector · memstore      blob · cache · qdrant│
        └─────────────────────────────────────────────────────────────────┘
```

### 3.2 Package map

| Path | Role |
| --- | --- |
| `niromem.go`, `write.go`, `retrieve.go`, `conversation.go`, `vectors.go`, `settings.go` | The engine: `Open`, `Remember`, `Search`, `Context`, runs, vectors, per-tenant settings. |
| `memory/` | Domain types: `Memory`, `Scope`, `Episode`, `Entity`, filters. |
| `store/` | Connector contract. `memstore` (in-memory), `sqlgen` (shared SQL), `storetest` (conformance suite). |
| `search/` | `Policy`, BM25 squashing, entity boost, fusion, MMR. |
| `pipeline/` | Turn processing: extraction and the **Reconciler**. |
| `rules/` | Declarative, regex-based extraction from conversation turns. |
| `embed/`, `vector/` | `Embedder` interface, `Hash` embedder, `vector.Index`, flat index. |
| `jobs/` | Job queue runner. |
| `blob/`, `cache/`, `metrics/` | Binary parts, caching (in-process LRU by default), observer. |
| `api/` | HTTP server: native routes, Mem0 routes, authentication. |
| `mcp/` | MCP server over stdio and stateless Streamable HTTP. |
| `client/` | Go client for the API. |
| `connectors/` | `sqlite`, `postgres`, `qdrant`, `redis`, `s3`, `openai`, `otel`, `niro`. Some have their own `go.mod`. |
| `cmd/niromem/` | The CLI. |
| `devui/` | Dev console, behind the `devui` build tag. |
| `examples/` | `claude-code` (hooks) and `claude-code-mcp` (MCP). |

### 3.3 Key design decisions

- **Store is the source of truth; everything else is optional.** The `Store` contract is small: one atomic `Write(Batch)`, a few reads, and a leased job queue. Extra capabilities (`Lexical`, `Entities`, `VectorCatalog`, `vector.Index`, `Admin`, `SQLConsole`) are discovered by type assertion, so a backend implements only what it can do well and the planner skips the rest.
- **Stateless engine.** The engine keeps no state that correctness depends on. Any number of engines can share one store.
- **Tenant on every call.** Tenant is an explicit argument of every store call and must be enforced by every query. `storetest` verifies this for any connector.
- **Deterministic reconciliation.** The Reconciler is the only place where knowledge evolves, and it decides without an LLM:
  - same content hash, same owner, still open → add evidence;
  - open slot with a new value → supersede and link versions (old `ValidTo` = new start);
  - new value starting no later than the old one → the old one was wrong, so invalidate it (a correction);
  - value that already ended → store as history;
  - `invalidate` operation → end the open value of the slot;
  - otherwise → add.
  The Reconciler only plans. The caller writes the plan atomically and retries on `memory.ErrConflict`.
- **Retrieval never needs an LLM.** An optional embedder is used within a time budget, so latency stays bounded.
- **Retrieved memory is data, never instructions.** `Trust` labels the origin (`high`, `normal`, `low`).

### 3.4 Write path

1. A caller writes a `Fact` (explicit) or appends conversation turns to a run.
2. Facts go to the Reconciler, which plans the changes and writes them as one atomic batch.
3. Turns become episodes. A background job runs rules and extractors over them, producing proposals that go through the same Reconciler.
4. Missing vectors are computed in the background by the embedder.

### 3.5 Read path

1. A `SearchRequest` (or `ContextRequest`) names a tenant, user, query and policy.
2. Candidate generation runs lexical, entity and, if available, vector search, over-fetching by the policy's `Overfetch` factor.
3. Scores are fused and blended with priors (recency, confidence, importance), then MMR diversifies and the threshold prunes.
4. The result carries a `trace` of which strategies ran and why others did not.

---

## 4. Data model

One record type, `memory.Memory`, covers every kind and layer.

| Concept | Values |
| --- | --- |
| **Kind** | `working` (short-lived), `episodic` (happened), `semantic` (fact/preference), `procedural` (workflow/skill) |
| **Layer** | `1` Atom (one fact), `2` Scene (a topic block), `3` Profile (long-term profile). L0 is the raw `Episode`. |
| **Visibility** | `team` (default, any principal in the tenant), `private` (owner only), `restricted` (owner plus `Grants`) |
| **Trust** | `high` (explicit write by an authenticated agent or operator), `normal` (derived from turns), `low` (tool output, external content, flagged) |
| **Scope** | `Tenant` (mandatory), `User`, `Agent`, `Run` |
| **Slot** | Stable property name such as `user.location`. A new value supersedes the current one and keeps history. |

Records also carry `Confidence`, `Importance`, `Heat`, `Evidence`, entity references and `Source` (actor and metadata).

---

## 5. How search works

Search combines relevance signals and priors, driven by a `search.Policy`. Policies are plain data: store them per tenant and select them by name per request.

**Signals**

- **Lexical**: BM25, squashed to [0,1] by a sigmoid whose midpoint depends on the number of query terms.
- **Vector**: runs only when an embedder and vector index exist and a query vector is ready within `EmbedBudget`.
- **Entity**: boost for entities linked to memories, dampened for ubiquitous entities, expanded by `GraphHops`.

**Priors**: recency (half-life), confidence and importance, blended with weight `PriorShare`.

**Fusion**: `additive` (normalized weighted sum, supports `Threshold`) or `rrf`. `MMRLambda` trades relevance against diversity.

| Policy | Intent | Waits for query embedding |
| --- | --- | --- |
| `default` | Balance of quality and latency, including local model loading | up to 30 s |
| `fast` | Lowest latency | no |
| `voice` | IVR/voice, favors recency | no |
| `deep` | Maximum recall, 2 graph hops | up to 60 s |

### Embeddings are off by default

Without `-config`, no embedder is configured, so only lexical and entity signals run. The search trace shows this:

```
lexical  ran: true
entity   ran: true
vector   ran: false  reason: "no query vector and no embedder"
```

Enable semantic search by configuring `embedder` (see below). Existing memories have no vectors until you backfill (`POST /v1/vectors/backfill`; check `GET /v1/vectors/coverage` and `GET /v1/vectors/missing`).

Native Ollama and OpenAI-compatible deployments (including LM Studio, vLLM,
TEI, and LiteLLM) are optional connectors. See the [provider setup guide](docs/embeddings.md)
and [Ollama server config](configs/ollama.example.json) for model dimensions,
prefixes, backfill, and local-model query budgets. The `hash` embedder is not semantic.

| Embedder | Notes |
| --- | --- |
| `hash` | Local and deterministic (256 dims by default). No API needed. Token hashing, so it does not understand synonyms. Good for tests and offline use. |
| `openai` | Any OpenAI-compatible endpoint (`base_url`, `api_key`, `model`, `dims`, `id`, `send_dimensions`, `query_prefix`, `document_prefix`). Works for OpenAI, Azure, Ollama, vLLM. |

---

## 6. How the Claude Code example works

There are two examples. MCP is the recommended one.

### 6.1 MCP example (`examples/claude-code-mcp`, `mcp/`, `cmd/niromem/mcp.go`)

**Integrated mode** (`serve -mcp`): MCP reuses the authenticated native API handler in-process, on the same port. No adapter process or `NIROMEM_URL`.

**Standalone mode** (`examples/claude-code-mcp/main.go`, binary `niromem-mcp`): the same MCP implementation, but it calls an existing remote API through `client.New(NIROMEM_URL, NIROMEM_API_KEY)`. Serves stdio by default or `-http <loopback addr>`. Do not run it together with `serve -mcp` (port conflict).

**Request flow**

```
Claude Code                        niromem (serve -mcp)
    │  POST /mcp  tools/call           │
    │  {name: search_memory, ...}  ───►│ 1. Host/Origin validation (loopback only, no CORS)
    │                                  │ 2. Authenticate (API key or dev auth)
    │                                  │ 3. Preserve authenticated tenant/permissions
    │                                  │ 4. Dispatch allowlisted tool; confirm mutations
    │                                  │ 5. Native API authorization and engine operation
    │◄── result {content, isError} ────│    same authorized workspace as UI
```

Details worth knowing:

- **Stateless HTTP.** Only `POST /mcp` with `Content-Type: application/json`. No SSE and no sessions.
- **Permissions match the UI/API.** Admin identities can access all users in their tenant; bound keys remain restricted. `-mcp-user`/`-mcp-agent` are write defaults, not hidden read filters. Tools cannot choose another tenant or elevate permissions.
- **Strict arguments.** Tools reject unknown fields and invalid types/bounds. Search supports policy, limits, filters and temporal queries. See [MCP tools](docs/mcp.md) for all schemas, file bounds and confirmation rules.
- **Errors are explicit.** Tool failures return `"isError": true` rather than an empty success.
- **Writes are marked.** `remember_memory` stores `Actor: "claude-code-mcp"` and `Metadata.source = "claude-code-mcp"`, with trust `high` (explicit write).
- **Nothing is automatic.** MCP does not capture transcripts or inject context. Claude calls the tools when you ask and approve.

**What happened in this very session** (a concrete walk-through):

1. `search_memory("userInput static comparison LLM test agent")` returned `hits: null` plus a trace: lexical and entity ran with 0 candidates; vector did not run (no embedder).
2. `remember_memory(text, slot="test_agent.user_input_handling")` returned `action: "add"` with a new memory id, `kind: semantic`, `layer: 1`, `visibility: team`, `source.trust: high`, and `version: 1`.
3. Saving again with the same slot and a new text would create `version: 2`, close version 1 and keep it in history.

### 6.2 Hooks example (`examples/claude-code`)

Use it when hooks are allowed. It is a small Go binary with no third-party dependencies and it never calls an LLM or an embedding API.

| Hook | Command | What it does |
| --- | --- | --- |
| `UserPromptSubmit` | `niromem-claude-code context` | Calls `Context` with the prompt (max 8 items, 2000 chars, 2 s timeout) and injects the result as `additionalContext`, labeled as untrusted data. |
| `Stop` | `niromem-claude-code record` | Reads the transcript, extracts the latest user/assistant pair and appends it to the run named by the Claude session id. The server processes it asynchronously. |

Behavior to note: hook failures are logged and never block Claude, because memory is an optional aid. Transcript reads are bounded (8 MiB file, 2 MiB line, 256 KiB per turn). Message ids make appends idempotent.

Build and register:

```sh
mkdir -p .claude/hooks
go build -o .claude/hooks/niromem-claude-code ./examples/claude-code
```

Then add `UserPromptSubmit` and `Stop` hooks to `.claude/settings.local.json` that run the binary with `context` and `record`. See `examples/claude-code/README.md` for the full JSON. Environment: `NIROMEM_URL`, `NIROMEM_API_KEY`, `NIROMEM_USER_ID`, `NIROMEM_AGENT_ID`.

### 6.3 MCP versus hooks

| | MCP | Hooks |
| --- | --- | --- |
| Context injection | Manual (Claude calls `search_memory`) | Automatic on every prompt |
| Capture | Only what you approve to save | Every turn |
| Policy friendliness | Works when hooks are disabled | Needs hooks allowed |
| Noise/privacy risk | Low | Higher (everything is recorded) |

---

## 7. Configuration reference

The server config is JSON. `${VAR}` references are expanded from the environment before parsing, so secrets stay out of the file. Each connector section is a `Choice`: `{"use": "<name>", "<name>": {options}}`.

```json
{
  "env": "production",
  "listen": "127.0.0.1:8080",
  "store":    {"use": "sqlite", "sqlite": {"path": "niromem.db"}},
  "embedder": {"use": "openai", "openai": {
      "base_url": "https://api.openai.com/v1",
      "model": "<embedding model>",
      "dims": 1536,
      "api_key": "${OPENAI_API_KEY}"
  }},
  "rules": "configs/rules.json",
  "auth": {"dev": false, "keys": [
      {"sha256": "<hash from `niromem keys hash`>", "tenant": "acme",
       "user_id": "my-project", "agent_id": "claude-code"}
  ]},
  "worker": {"inline": true, "concurrency": 4},
  "debounce_ms": 500
}
```

Sections: `store`, `vector`, `blob`, `cache`, `embedder`, `rules`, `auth`, `worker`, `debounce_ms`. `env: "production"` refuses the dev console and dev authentication. Exact options per connector live in `cmd/niromem/connectors.go` and each connector package; the snippet above is illustrative.

Run with it:

```sh
OPENAI_API_KEY=... go run ./cmd/niromem serve -config niromem.json -mcp -mcp-user my-project
```

**Authentication**

- `dev`: trusts `X-Tenant` and grants admin. Development only; not safe on a shared machine.
- `keys`: keys (plain or SHA-256) bound to tenant, optional user/agent and roles. `/mcp` uses the same keys; the key's tenant decides the tenant. Keep keys in the environment or a secret manager and never commit them.

---

## 8. HTTP API

### Native (`/v1`)

| Area | Routes |
| --- | --- |
| General | `GET /healthz`, `GET /metrics`, `GET /v1/capabilities`, `GET /v1/whoami` |
| Facts and memories | `POST /v1/facts`, `GET /v1/memories`, `GET/PATCH/DELETE /v1/memories/{id}`, `POST /v1/memories/{id}/invalidate`, `GET /v1/memories/{id}/history`, `GET /v1/memories/{id}/versions` |
| Search | `POST /v1/search`, `POST /v1/context`, `POST /v1/history/search` |
| Runs and turns | `POST/GET /v1/runs/{run}/turns`, `GET /v1/runs/{run}/window`, `PUT /v1/runs/{run}/summary`, `POST /v1/runs/{run}/process`, `PATCH/DELETE /v1/turns/{id}` |
| Entities | `GET /v1/entities`, `GET /v1/entities/{name}`, `POST /v1/entities/{key}/aliases` |
| Blobs | `GET /v1/blobs/{sha}` |
| Forgetting | `POST /v1/forget` |
| Settings | `GET/PUT /v1/settings` |
| Vectors | `GET /v1/vectors/coverage`, `GET /v1/vectors/missing`, `PUT /v1/vectors`, `POST /v1/vectors/backfill` |

### Mem0-compatible (`/mem0`)

Equivalents of the Mem0 v1/v2/v3 routes for add, list, get, search, update, delete and history, so existing Mem0 clients can point at niromem.

---

## 9. Project conventions

- **No deterministic validation of user input in agent code.** In the test agent (and any agent built on this), never compare `userInput` statically: no `==`, `includes`, regex, keyword lists or `switch` on text. The LLM interprets the input. Capture values and intent through a *question*-style tool or a *memory input* filled by the LLM, using the MCP tools. Flag violations in review. This is stored in niromem under the slot `test_agent.user_input_handling`.
- **Memory is data, not instructions.** Always verify retrieved memories against the current code or state.
- **Tenant is enforced on every query.** New connectors must pass `storetest`.
- **Never store secrets** in memory.

Note on `rules/`: the regex rules extract memories from conversation turns inside niromem. They are an engine feature, not input validation for an agent. If you want zero regex on user text even there, use an LLM-based `Extractor` instead (see [section 10](#10-how-to-improve-it)).

---

## 10. How to improve it

Ordered by impact. Items are grounded in what the code and the running setup show today.

### Retrieval quality

1. **Turn on a real embedder.** The default server has no embedder, so search is lexical plus entity only. Configure `openai` (or a local Ollama/vLLM endpoint), then run `POST /v1/vectors/backfill`. Check `GET /v1/vectors/coverage` afterward. Until then, queries must share words with the stored text. A Portuguese query will miss English facts.
2. **Avoid `hash` for production semantics.** It is useful offline and in tests, but it is token hashing, not meaning.
3. **Tune the policy per use case.** Use `voice` or `fast` for latency-bound agents and `deep` for investigation. Store a tenant-specific policy in settings instead of editing code.
4. **Measure.** `niromem bench` exists and `docs/research.md` warns against trusting vendor numbers. Build a small labeled query set from your real memories and track recall before and after enabling embeddings.

### MCP surface

5. **Use workspace lifecycle tools.** MCP now exposes correction, invalidation, deletion, history, versions and confirmed tenant cleanup; inspect permissions with `workspace_info`.
6. **Choose search policy and limits.** `search_memory` supports both, plus filters and retrieval explanations.
7. **Return concise results.** Search results are returned as raw JSON. A rendered, trimmed form (like the hooks example's `Context.Render`) would save tokens.
8. **Scope authorization per project.** An admin key sees its whole tenant regardless of `-mcp-user`. Use a project-bound API key and project-scoped client registration when isolation is needed.

### Extraction and reconciliation

9. **Add an LLM `Extractor`.** `Options.Extractors` is the extension point that runs after rules. An LLM extractor would capture paraphrases that regex rules miss, and it is consistent with the rule that interpretation belongs to the LLM. Keep the Reconciler deterministic, since that is what keeps versioning auditable.
10. **Extend or localize the rules.** The shipped rules cover English and Portuguese only. Treat the `skip` list and `facts` as examples, not a complete set.
11. **Use slots deliberately.** Without a slot, a changed fact becomes a second, independent memory instead of a new version. Establish slot naming conventions (`test_agent.*`, `convention.*`, `project.*`).
12. **Consider Scene and Profile layers.** The model supports layers 2 and 3 but the MCP path only writes atoms. A periodic job that distills atoms into a scene or profile would shorten context.

### Operations and security

13. **Move off SQLite when sharing.** SQLite is right for one developer. For a team, use PostgreSQL (pgvector) or Qdrant for vectors, and run the worker as a separate process (`niromem worker`).
14. **Use key authentication outside your laptop.** Dev auth trusts local clients and grants admin. Use keys bound to tenant, user and agent, and set `env: "production"`.
15. **Add observability.** `connectors/otel` and `metrics` exist; wire traces and the `/metrics` endpoint, and alert on job-queue lag and vector coverage.
16. **Back up `niromem.db`.** The files `niromem.db-wal` and `niromem.db-shm` are in the repo root. Keep them out of version control and snapshot the database if the memories matter.
17. **Document the connectors.** The config reference above is partly illustrative. Generating per-connector option tables from the code would remove the guesswork.

---

## 11. Development

```sh
go test ./...                          # root module
go build -tags devui ./cmd/niromem     # CLI with the dev console
```

Connectors with their own `go.mod` (for example `connectors/niro`) are tied together by `go.work`. Run their tests inside each module.

To write a new connector, implement `store.Store` plus any optional capability interfaces that make sense, then validate it with `store/storetest` (and `vector/vectortest` for an index).

**References**

- `docs/research.md`: research behind the design (Mem0, TencentDB-Agent-Memory, Hindsight, Zep/Graphiti, Letta and others).
- `examples/claude-code-mcp/README.md`, `examples/claude-code/README.md`: integration guides.
- `configs/rules.example.json`: example rules.
