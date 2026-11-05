# Document ingestion

The Remember import cards and Operations job groups offer **Preview file** for
the original while processing runs. Jobs from the same file on a bounded queue
page appear under one expandable file card. See [Operations](./operations.md)
for preview formats and queue grouping limits.

The optional `connectors/documents` Go module extracts text locally from PDF,
TXT, Markdown and DOCX. It includes local OCR for PDF pages without text.
The core depends only on `DocumentParser`, not Poppler, Tesseract or a PDF SDK.
The CLI adapts the module into the existing API and leased job worker.

## Configuration and dependencies

```json
{
  "blob": {"use": "dir", "dir": {"path": "blobs"}},
  "documents": {"enabled": true, "concurrency": 2}
}
```

Use persistent blob storage shared by every server/worker, such as a shared
directory or the existing S3 connector. Process-local blob storage is suitable
only for tests. Install `poppler-utils` for PDF and `tesseract-ocr` for scans on
every worker. Missing utilities or unreadable files produce explicit job errors;
they never create invented text or a successful empty import.
The implementation is Go plus native Poppler/Tesseract executables; Python is
not used or required. OCR currently uses Tesseract's default English language.

`documents.concurrency` is 1..4 (default 2) and bounds page OCR per document.
The existing `worker.concurrency` bounds simultaneous durable jobs. The UI
uploads at most two files concurrently. On low hardware, keep one model worker;
more concurrent model requests often make CPU inference slower, not faster.

## One action, asynchronous stages

1. **Remember -> Import documents** accepts up to ten selected files.
2. Each upload validates the format, stores content-addressed original bytes,
   and atomically records its source episode, queued job and upload receipt.
3. `document.import` extracts PDF text or OCRs textless pages. DOCX extracts
   paragraphs, not rendered page layouts. TXT/Markdown retain their text.
4. The worker persists the exact parsed-text snapshot before committing chunks.
   A retry reuses that snapshot rather than reinterpreting a changed document.
5. Source-linked chunks, text evidence episodes, history events, completion
   receipt and background embedding/enrichment jobs commit atomically.
6. `ingested` means source chunks are saved, **not** that all model work has
   finished. Inspect **Operations -> Background jobs** for model progress.

With an embedder configured, normal batched embedding jobs make chunks
semantically searchable. `knowledge.background_enrichment: true` schedules the
selected enrichment contract. In `extractive_only` mode, document jobs report
`source_only` with a reason: indexed passages already provide the original text,
so no selection-model call or duplicate excerpt memories are created. Structured
planners retain their grounded entity/claim/relationship extraction. Disabled mode preserves
chunks without extraction; shadow mode records suggestions without applying
them; enforced mode adds unreviewed derived knowledge. An import does not change
tenant policy or automatically override existing assertions.

## Passage boundaries and section context

New imports split at Markdown and dotted-number section headings (such as
`5.2.1.1`) before grouping sentences and paragraphs toward 1800 UTF-8 bytes.
Passages never mix detected sections. The hard limit remains 3500 bytes;
an oversized sentence without a usable boundary must still be split, with an
explicit `continuation` marker. Wrapped lines alone are not sentence boundaries.
Heading detection is heuristic, not a reconstruction of a PDF's visual layout.

Each passage carries `section`, `section_title`, `page_start`, `page_end` and
`continuation` metadata. Page byte offsets refer to the unchanged parser output.
The section title is also the memory title, so continuation passages receive
context in lexical and embedding retrieval. Section context carries across pages.
Explicit section titles and numbers are indexed as `document_section` entity
references, without a model or invented semantic relationships. This helps
literal section/topic lookup; it is not a general entity-extraction graph.
Chunks containing only headings/URLs are marked `navigation_only` and
downweighted in retrieval, not deleted.

Existing imports are not rechunked automatically; their durable receipts remain
idempotent. Use an explicitly requested processing revision to rebuild boundaries
without deleting the original or its processing history.

## Document identity and lineage

**Memories -> Documents & lineage** (`#/documents`) opens
the development-only document library. Select an import to preview/download its
authenticated original, inspect four provenance layers and browse revisions:

1. **Original**: immutable uploaded bytes and source episode; the document ID,
   not the filename, is its identity.
2. **Chunks**: source text with page/section context and original evidence spans.
3. **Structured knowledge**: grounded, **unreviewed** assertions, entity types and
   attributes; these are interpretations, not authoritative corrections.
4. **Embeddings**: actual persisted vectors attached to those records, including
   model ID, dimensions and whether they match the current query model.

Revision-specific entity classifications/attributes live in `extracted_entities`
annotations. The shared entity catalogue still resolves names to canonical
entities; reprocessing does not rewrite a catalogue type used by other documents
or historical revisions.

Solid graph edges describe deterministic provenance. Inferred semantic edges are
distinguished and shown only when both authorized endpoints are on the page.
Selecting a node opens its inspector; full text/evidence is available through
the memory link. The graph displays at most 80 nodes/200 edges; paginated records
remain available below it. A missing persisted embedding is explicitly reported,
not inferred from a configured model or an overall coverage percentage.

New chunks also reference the document as a `document` entity. Existing imports
and legacy extraction source links can be inspected read-only without executing
models. That reconstruction does not silently backfill the old entity index.
Legacy ancestry lookup permits four authorized hops; original-derived discovery
is bounded to 4096 same-owner enrichment records and fails explicitly beyond
that bound rather than claiming exhaustive lineage.

## Manual incremental revisions

Configuration changes do **not** automatically reprocess documents. An admin
explicitly selects stages in the document page or requests:

```http
POST /v1/documents/DOCUMENT_ID/reprocess
Content-Type: application/json

{"request_id":"stable-approved-request","stages":["knowledge","embeddings"]}
```

Stages are `chunks`, `knowledge`, `embeddings` and `relationships`. Selected
stages recompute; unselected assets and vector spaces are copied to the new
snapshot without model calls. Rechunking requires `embeddings` when an embedder
is configured, and `knowledge` when enriched records already exist.
`relationships` requires a separately configured inference provider; structured
extraction itself also produces fact-to-source `derived_from` links.
Inference candidates are bounded to 20 other records in the same snapshot.
Existing links to current, same-owner/same-private-audience external records
are preserved with rewritten document endpoints, not silently discarded.
Changed or inaccessible external endpoints require review.

The original import must have finished. Knowledge activation requires an
enforced, structured enrichment provider and obeys the configured admission
policy; an excerpt-only provider cannot masquerade as structured extraction.
Media reinterpretation is not supported by the `chunks` stage. Revisions require
target-level vector reads (provided by memstore, SQLite and PostgreSQL).

`document.reprocess` is a durable leased job. Drafts, each completed source-chunk
extraction, and vector/relationship batches are persisted as operation receipts; checkpoints expose
`stage`, `completed` and `total`. A restart/retry reuses completed work.
An interrupted chunk resumes from its last chunk checkpoint; individual evidence-window
calls within that chunk are not separate durable checkpoints.
Extraction validates facts and entity annotations independently. A malformed
entity cannot discard a grounded fact or stop processing the rest of the
document. Dense text is decomposed into small evidence windows; rejected
records carry explicit `issues` instead of being silently dropped.
If any passages still fail extraction, the revision activates its validated
assets with **`incomplete:true`**, per-source issue details, claim/empty-source
counts, and a `repair_revision_id`. This is not a complete extraction.

Activation atomically queues one `document.repair` job. It creates a successor
revision that re-extracts **only failed chunks**, retaining healthy structured
items and already-persisted embeddings. Within a failed chunk, evidence windows
are evaluated again; valid previous assertions are retained and deduplicated.
If the active revision changed in the meantime, the repair does not replace it.
There is at most one automatic repair round, not an infinite model retry loop.
Remaining gaps stay visible for manual review/reprocessing.

Transport failures without usable partial output remain retryable. Invalid
vector output, admission refusal, changed model/schema/knowledge mode or stale
source state produces **`needs_review`**, leaving the prior revision active.
Correct the cause and create a new request ID for a new candidate revision.

After validation, one atomic transaction closes the
previous assets/relationships, inserts the new snapshot and switches the
versioned document manifest. Recording timestamps reflect activation, not
queueing. The original blob/episode never changes. Prior committed revisions
remain inspectable; `committed` does not mean “still current”—consult `is_current`
on the graph, and inspect `incomplete` before treating extraction as finished.
External vector indexing uses the existing transactional outbox.
Ordinary retrieval and embedding coverage exclude internal document manifests.

Reuse the same request ID and identical stage selection after an uncertain
response; changing stages with that ID is a conflict. Competing activations
cannot silently overwrite each other. Current snapshots are bounded to 4096
assets and relationship reads to the connector's explicit read limit.

## Limits and safety

- Each file: 1 byte to 20 MiB; PDF: at most 100 pages.
- Parsed text: at most 2 MiB; source chunks: at most 3500 UTF-8 bytes.
- Parsing is bounded by the connector timeout; temporary OCR files are removed.
- Encrypted/corrupt PDFs, invalid DOCX, unsupported formats and scans that cannot
  be OCRed fail explicitly. OCR is imperfect; original files remain evidence.
- Source chunks are private to the supplied user/agent scope. Tenant and bound
  identity restrictions apply to upload, listing, status and original download.
- Each chunk carries document ID, filename, page/chunk numbers and a byte-range
  evidence reference. Source chunks are not merged into differently visible
  pre-existing assertions.

Identical content, filename and scope produce the same import ID. Re-uploading
is idempotent and does not enqueue duplicate imports. Processing leases and
durable receipts permit restart/retry on another configured worker. Retry a
failed job through Operations after correcting a dependency issue. Deleting a
chunk does not automatically reimport it or delete the original document.

## HTTP API

All routes use the existing API authentication and tenant conventions.

`POST /v1/documents` accepts JSON and returns HTTP 202:

```json
{"name":"manual.pdf","bytes":"BASE64_FILE_BYTES","user_id":"my-project"}
```

Optional `agent_id` follows existing scope rules. Response:

```json
{"id":"doc-...","name":"manual.pdf","status":"queued","pages":0,"memories":[]}
```

- `GET /v1/documents/{id}`: durable import status; may report queued,
  processing, failed or ingested, with explicit last job error.
- `GET /v1/documents/{id}/file`: authenticated original download.
- `GET /v1/documents/{id}/preview`: authenticated inline preview for supported
  PDF/text/image/audio/video types; unsupported formats require download.
- `GET /v1/documents/{id}/graph?revision_id=...&after_id=...&limit=20`:
  document provenance and persisted embedding metadata, never vector coordinates.
  Omit `revision_id` for current, use `original` for the first import or a
  committed revision ID for history. Asset pages accept limits 1..50.
- `GET /v1/memories/{id}/document`: authorized source-document lookup.
- `GET /v1/documents/{id}/revisions`: latest 20 manual requests and statuses.
  Add `revision_id` to inspect one request, its job error and durable progress.
- `POST /v1/documents/{id}/reprocess`: admin-only asynchronous revision request.
- `GET /v1/documents?user_id=...`: up to twenty recent imports found among the
  latest two hundred user-role source episodes in the authorized scope. It is
  a bounded development view, not an exhaustive document export.

Completion returns page count and IDs of saved source chunks. Knowledge results
remain available through the existing enrichment/state/transition APIs.
