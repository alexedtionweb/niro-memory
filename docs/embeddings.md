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

Existing memories need vectors too: open **Embeddings**, run **Backfill missing**,
and monitor **Jobs** and model coverage. New writes queue embeddings automatically.
Do not expect semantic recall until the relevant memories have vectors.

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
embeddings have a policy budget. The default budget is 50 ms; `deep` allows
500 ms. A local model's cold start can exceed either, causing the vector
strategy to be skipped for that request. This is visible in the search trace;
the query embedding can be cached for a subsequent search.

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
      "mmr_lambda": 0.7, "embed_budget": 2000000000,
      "overfetch": 4, "graph_hops": 1
    }
  },
  "default_policy": "local-semantic"
}
```

Durations are nanoseconds (the example waits up to two seconds). MCP uses a
five-second tool deadline, so keep its query budget below that. No strict
10 ms target is required; choose a measured quality/latency trade-off.

Do not mix vectors from different providers, models, dimensions, or prefix
schemes in one model ID. Use a new revision ID, backfill it, verify coverage,
and then activate it. The configured embedder is implicitly declared; a tenant's
explicit active model can override it, so check both model activation and coverage.
Supplying vectors directly remains supported without any embedder.
