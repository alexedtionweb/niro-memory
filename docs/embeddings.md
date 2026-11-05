# Optional embedding providers

The core depends on `embed.Embedder`, not a provider SDK. Providers remain
separate modules; import only the connectors your application uses. The supplied
CLI composes them explicitly and instantiates only the configured embedder.
The server does not install models, start model servers, or download weights.

| Backend | Connector / protocol | Notes |
| --- | --- | --- |
| Ollama | `connectors/ollama`, native `/api/embed` | Local batches, keep-alive, optional dimension reduction, explicit truncation policy |
| OpenAI | `connectors/openai`, `/v1/embeddings` | Bearer key; optional `dimensions` |
| LM Studio | `connectors/openai`, OpenAI-compatible API | Load an embedding model and use its actual model ID and dimensions |
| vLLM | `connectors/openai`, OpenAI-compatible API | Serve an embedding-capable model |
| Hugging Face TEI | `connectors/openai`, OpenAI-compatible API | Use the deployment's compatible `/v1` base URL |
| LiteLLM | `connectors/openai`, OpenAI-compatible proxy | Provider routing and credentials belong to the proxy |
| Azure OpenAI | `connectors/openai`, compatible v1 API | Use your deployment's v1 base URL, model/deployment ID, and required headers |
| Other providers | Implement `embed.Embedder` or use a compatible gateway | Cohere, Voyage, and Bedrock native protocols are **not** implemented by these connectors |

Compatibility depends on the deployed service exposing the stated embeddings
protocol. Connector tests use controlled HTTP fixtures, not certification of
every hosted service/version. `hash` is only a deterministic development/test
embedder: it is **not semantic**.

## Ollama with the all-in-one server

Start your installed Ollama service and explicitly install the chosen model,
according to your organization's model and data policies:

```sh
ollama pull nomic-embed-text
go run -tags devui ./cmd/niromem serve \
  -config configs/ollama.example.json -dev-ui -mcp -mcp-user my-project
```

The example uses the model's 768-dimensional space and its query/document
prefixes. Change both dimensions and prefixes to match your chosen model.
Default `truncate:false` rejects overlong inputs rather than silently embedding
only part of a memory. Such failures appear in the background jobs.
`send_dimensions:true` should be used only with a model/server supporting it.

The niromem API, UI, and MCP still run in one process on port 8080. Ollama is an
optional external model service, not a core dependency. Models can take longer
on their first request; keep-alive can reduce subsequent model-loading overhead.

The Ollama embedder's `timeout_ms` sets the HTTP timeout **per batch**, including
time waiting for response headers, model loading and response reads. Zero or an
omitted value uses five minutes (previously a hidden 60 seconds). The LFM example
configs explicitly allow ten minutes with `"timeout_ms":600000`. Restart niromem
after changing this setting. Go callers can set `ollama.Options.Timeout`; an
explicit `Options.HTTP` client retains its own timeout unchanged.
Caller cancellation, search policy budgets and worker deadlines still take
precedence; raising this ceiling does not add latency to successful requests.
Timeout errors retain their cause and report the model, batch size and HTTP
timeout. Durable document embedding jobs can retry; queries retain their
bounded wait and report skipped vector retrieval when the budget expires.

An HTTP timeout while awaiting headers does not establish that vector
computation itself is slow: a cold model or Ollama queue shared with generation
may delay the request. Keeping the model loaded avoids reloads but cannot
guarantee low latency under contention. For predictable query latency, serve
embeddings on a dedicated Ollama instance and point `embedder.ollama.base_url`
at it, or reduce competing ingestion/generation load. Retain the same model,
dimensions, ID and prefixes to preserve the existing vector space.

Existing memories need vectors too: open **Models & schemas -> Embeddings**,
run **Embed existing memories**, and watch the actual coverage and job status.
The UI shows the effective active query model and dimensions, including the
implicitly declared server embedder. Backfill is a durable job; keep the worker
running (or use the example's inline worker). New writes queue embeddings automatically.
Do not expect semantic recall until the relevant memories have vectors.

## EmbeddingGemma in the text-only LFM example

The [LFM2.5 configuration](../configs/lfm2.5-350m.example.json) uses the local
`embeddinggemma-2:440m` Ollama model: 768-dimensional output, separate model ID
`ollama/embeddinggemma-2:440m-chunk1024-mean@1`, query prefix
`task: search result | query: ` and document prefix `title: none | text: `.
These retrieval instructions follow the
[EmbeddingGemma 2 model card](https://huggingface.co/google/embeddinggemma-2).
The generic connector has no separate title field, so it uses `title: none`;
any stored title remains part of the source text supplied by the engine.
The model is multilingual; retrieval quality still requires evaluation on your
own data. `truncate:false` remains enabled.
`chunk_size:1024` splits source text at UTF-8 character boundaries into at most
1024-byte chunks. Each chunk receives the appropriate query/document prefix.
If Ollama explicitly rejects a chunk for exceeding context, only that input is
split further; unrelated errors propagate without splitting or silent fallback.
No source bytes are dropped, including whitespace and the end of long inputs.

Chunk vectors are combined into one vector per original input using a
source-character-count-weighted mean and L2 normalization. Single-chunk vectors
are unchanged. This preserves the existing one-vector-per-memory storage shape,
but a pooled vector is not passage-level retrieval and may dilute small details
in a large memory. Both queries and documents use the same chunking strategy.
Chunking is opt-in for other Ollama configs (`chunk_size:0` disables it) and cannot
be combined with `truncate:true`. A request allows at most 4096 initial chunks;
larger inputs/batches fail explicitly. Caller deadlines still cancel all work.
This changes the embedding strategy, so the new model ID separates these vectors
from both Nomic and earlier BGE vectors. Go callers omitting an explicit
ID get a chunk-size-specific default ID; callers specifying an ID must change it
when changing their chunk strategy.

Restart with this config and explicitly run **Embed existing memories** for
the EmbeddingGemma model. Existing Nomic/BGE vectors and memories are not deleted or converted,
and changing the config does not automatically start a backfill. Verify coverage
and check any tenant active-model overrides before relying on semantic search.
The LFM3-Omni and generic Ollama configurations still use Nomic.

## OpenAI-compatible deployments

Use the `openai` connector for compatible services instead of inventing a new
connector for each provider name. Example embedder section for LM Studio:

```json
{
  "embedder": {
    "use": "openai",
    "openai": {
      "base_url": "http://127.0.0.1:1234/v1",
      "model": "<loaded-embedding-model>",
      "dims": 768,
      "id": "lmstudio/my-embedding-space@1",
      "batch_size": 32
    }
  }
}
```

Merge the section into a complete server config; placeholders must be replaced
with actual model names and dimensions. For a remote endpoint, set `api_key`
using `${EMBEDDING_API_KEY}` expansion. Optional `headers` is a map of string
arrays, for example `"headers":{"api-key":["${AZURE_API_KEY}"]}`. Do not commit
real keys. Use HTTPS for remote services and send only approved data.
`query_prefix`/`document_prefix`, `send_dimensions`, `batch_size`, and
`max_retries` are configurable. The connector appends `/embeddings` to
`base_url`; do not include `/embeddings` in that base.

## Query budgets and model migration

Embeddings are generated in background jobs for stored memories, while query
embeddings have a policy budget. The built-in `default` waits up to 30 seconds;
`deep` allows 60 seconds for cold local models. These are maximum waits, not
delays added to every request; cached embeddings return immediately.
`fast` and `voice` never wait for query embedding generation.
Exceeding the budget skips the vector strategy for that request, explicitly
reported in the search trace; authorized lexical/entity matches still return.
Background query generation/cache warming has at least the selected budget
(minimum ten seconds), rather than a shorter hidden generation deadline.

For slower/local models, create a tenant policy in **Tenants & settings** with
a more realistic budget. Preserve your existing settings and add:

```json
{
  "policies": {
    "local-semantic": {
      "name": "local-semantic", "fusion": "additive",
      "lexical": 1, "vector": 1, "entity": 0.5,
      "prior_share": 0.2, "recency": 1, "confidence": 1, "importance": 1,
      "recency_half_life": 2592000000000000,
      "mmr_lambda": 0.7, "embed_budget": 30000000000,
      "overfetch": 4, "graph_hops": 1
    }
  },
  "default_policy": "local-semantic"
}
```

Durations are nanoseconds (the example waits up to 30 seconds). Persisted tenant
policy overrides are preserved; an existing 50000000 value still means 50 ms
and must be updated explicitly. MCP search uses a 75-second tool deadline;
other MCP tools retain five seconds. The standalone MCP adapter's HTTP budgets
also allow the longer search. Client cancellation/shorter deadlines still win.
Keep custom query budgets below the tool and embedding-provider HTTP deadlines.
No strict
10 ms target is required; choose a measured quality/latency trade-off.

Do not mix vectors from different providers, models, dimensions, or prefix
schemes in one model ID. Use a new revision ID, backfill it, verify coverage,
and then activate it. The configured embedder is implicitly declared; a tenant's
explicit active model can override it, so check both model activation and coverage.
Supplying vectors directly remains supported without any embedder.
