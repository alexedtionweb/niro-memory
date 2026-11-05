# MCP workspace tools

The integrated `niromem serve -mcp` endpoint and the standalone
`examples/claude-code-mcp` adapter expose the same workspace tools. They work
without the development UI build tag. Restart the server/adapter and reconnect
your MCP client (refresh its tool list) after upgrading.

## Identity and access

MCP uses the same authenticated tenant and permissions as the native API/UI.
An admin key or local development identity can read all users' memories in its
tenant. A bound non-admin key retains its user/agent and visibility restrictions.
There is **no elevation** to admin and no tool argument for selecting a tenant.
Development authentication is intentionally powerful: only use it on a trusted
local machine. Use a scoped API key for less privileged LLM access.

`-mcp-user` and `-mcp-agent` select default write ownership, **not hidden read
filters**. Bound key identities override write defaults. Optional `user_id`,
`agent_id` and `run_id` filters are still checked by the API. Omit them to match
what that identity can see in the UI. Separate project names alone are not an
authorization boundary for an admin key.

### Matching the development UI tenant

The UI's tenant selector sends `X-Tenant` on development API requests. The
integrated HTTP MCP endpoint accepts the same header. Without it, development
authentication uses tenant `dev`, regardless of the tenant currently selected
in your browser. Browser local storage is not shared with MCP clients.
Configure the HTTP MCP client's headers, for example:

```json
{
  "mcpServers": {
    "niromem": {
      "type": "http",
      "url": "http://127.0.0.1:8080/mcp",
      "headers": {"X-Tenant": "YOUR_UI_TENANT"}
    }
  }
}
```

Reconnect after editing client configuration, call `workspace_info`, then
`embedding_status` without a model override. Verify identity, model ID and
coverage match the UI. Coverage counts all current memories in that tenant,
not only one document. Different tenants can correctly report 0/30 and 130/130
for the same embedding model. Do not start a backfill in a different tenant
just to match another tenant's progress. In API-key mode, use a key bound to
the desired tenant: `X-Tenant` does not override the key's authorization.

Integrated MCP authenticates every request, then invokes the native API handler
in-process with that identity. The standalone adapter authenticates native API
requests with `NIROMEM_API_KEY`; it does not depend on development endpoints.
Applications using `mcp.New` retain the older project-scoped tools;
`mcp.NewWorkspace` explicitly opts into workspace tools.

## Tool catalogue

Call `workspace_info` first for the actual identity, capabilities, effective
embedding models, knowledge permissions and extraction JSON schema.
`tools/list` provides strict argument schemas, bounds and mutation annotations.

| Area | Tools |
|---|---|
| Recall | `search_memory`, `list_memories`, `get_memory` |
| Lifecycle | `remember_memory`, `update_memory`, `invalidate_memory`, `delete_memory`, `memory_history`, `memory_versions` |
| Knowledge | `inspect_knowledge`, `inspect_enrichment`, `explain_memory`, `propose_memory_update`, `commit_memory_update` |
| Graph | `list_entities`, `entity_neighbors`, `memory_relationships`, `add_entity_alias`, `backfill_relationships` |
| Files | `import_document`, `import_media`, `list_documents`, `document_status`, `document_graph`, `document_revisions`, `memory_document`, `reprocess_document` |
| Jobs | `list_jobs`, `control_job` |
| Models | `embedding_status`, `backfill_embeddings` |
| Tenant administration | `get_settings`, `put_settings`, `cleanup_status`, `delete_all_memories` |

Search supports `query`, `policy`, `limit` (1..100, default 5), `threshold`,
`filter`, temporal/version options and optional owner filters. The default
`response_mode: "compact"` returns IDs, titles, source references and previews
of at most 1200 Unicode characters per hit. `text_max_chars` accepts 128..4000.
Use `get_memory` for complete evidence. Preview truncation is explicit.
`response_mode: "full"` returns native hit fields. Both modes are bounded to
24,000 serialized JSON bytes, preserve the strategy trace and report
`omitted_hits` when the requested results exceed that budget. Narrow the query
or request fewer hits rather than treating omitted results as absent.

Per-hit explanations are opt-in (`explain: true`); trace is always returned.
Scores are ranking signals, not confidence probabilities. Document import time
does not contribute to recency priors. Navigation-only chunks are downweighted;
plain extractive copies are collapsed when their authorized, matching source
is also a candidate. Exact ID lookup remains unchanged.
Inspect `trace`: missing embeddings or a skipped strategy
must not be interpreted as proof that no memory exists. Default query embedding
budget is 30 seconds; `deep` permits 60 seconds. Search and knowledge
proposal/commit tools have a 75-second deadline; other tools have five seconds.
Shorter caller deadlines still apply. Long model inference may exceed the MCP
planning deadline; ordinary saves and file uploads queue durable work instead.

`list_memories` accepts `before_id`; use the last ID of a full page to continue.
It preserves filters and does not use growing offsets. `list_jobs` accepts
`id`, `state`, `kind`, `limit` and `after_id`; its response includes
`next_after_id`. Jobs are always current-tenant and admin-only. No MCP tool
starts a competing manual worker, executes SQL, reads server filesystem paths,
or fetches arbitrary URLs.

## Mutations and confirmation

Corrections, invalidation, deletion, aliases, job controls, backfills, settings
replacement, document reprocessing and knowledge commits require `confirm:true`. Set it **only after
explicit user approval**. This argument guard is not an independent human
authentication mechanism: an authorized LLM can supply it. Use a restricted
API identity if the model must not administer data.

Examples of approved operations:

```json
{"name":"delete_memory","arguments":{"id":"MEMORY_ID","confirm":true}}
{"name":"control_job","arguments":{"id":"JOB_ID","action":"retry","confirm":true}}
{"name":"control_job","arguments":{"id":"JOB_ID","action":"priority","priority":50,"confirm":true}}
{"name":"reprocess_document","arguments":{"id":"DOCUMENT_ID","request_id":"stable-approved-revision","stages":["knowledge","embeddings"],"confirm":true}}
{"name":"delete_all_memories","arguments":{"confirmation":"EXACT_AUTHENTICATED_TENANT","request_id":"stable-user-approved-cleanup-key","confirm":true}}
```

Tenant cleanup also validates the exact authenticated tenant name. It is
irreversible and returns durable progress; poll `cleanup_status`. Reuse the
same `request_id` after an uncertain response. Never start a new cleanup merely
because a response was lost.

`put_settings` replaces the complete settings object. Read `get_settings` first,
preserve unrelated fields and confirm the reviewed result.
For corrections, pass `patch.operation_key`; knowledge proposals/commits use
`operation_key`. Retry with identical input after uncertain responses.
Saving an explicitly approved fact and uploading approved evidence do not
require an extra confirmation argument.

## Documents and multimodal evidence

Uploads accept `name` and original `bytes` encoded as base64, at most 16 MiB
decoded per MCP upload. HTTP and stdio accept envelopes up to 25 MiB. The native
API has its own file limits; larger files should use that API. No file path or
URL is accepted as an upload source. Avoid wasting LLM context on large base64
strings; upload through the API/UI and give the model the returned document ID.

An upload returns immediately with a durable import ID. Use `document_status`
to obtain processing errors, model provenance and resulting source memory IDs;
use `get_memory` to read extracted text and `inspect_enrichment` for claims.
Image/audio/video interpretation requires a configured media provider and remains
low-trust, unreviewed output. This is not a native multimodal vector index.

`document_graph` accepts `id`, optional `revision_id`, `after_id` and `limit`
(1..50). It returns source chunks, structured items, typed provenance links and
**persisted** embedding metadata across models, plus authenticated original
preview/download URLs. Coordinates are omitted. Omit the revision for current,
or use `original`/a committed revision ID for history. Graph counts are not a
substitute for reading evidence. `memory_document` resolves a memory's authorized
source document, including supported legacy enrichment links.

`document_revisions` lists the latest 20 manual requests; add `revision_id` for
one status and saved stage progress. `reprocess_document` is admin-only and
queues durable work, rather than performing inference within the MCP deadline.
It requires approval, a stable request ID and selected stages. Drafts stay out of
retrieval; successful activation atomically switches the document snapshot.
Malformed extraction records are isolated; validated partial results may activate
with `incomplete:true`, explicit per-source issues and one queued targeted repair.
`empty_sources` and `extracted_facts` distinguish empty model output from extraction
failures. Invalid configuration, vector output or stale activation still enters
`needs_review` without replacing existing knowledge. Nothing auto-reprocesses
merely because settings change.
See [document revisions](./documents.md#manual-incremental-revisions) for stage
dependencies, copying versus recomputation, history and limits.

Tool results preserve API response shapes and explicit `isError` failures.
Individual responses over 2 MiB are rejected with an instruction to narrow the
query; a mutation may already have committed, so inspect its status before
retrying. Original binary preview/download remains in the API/UI rather than
being dumped into a model's context.
