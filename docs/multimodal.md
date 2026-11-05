# Multimodal memory

The optional `connectors/multimodal` Go module interprets images, audio and short
videos through the configured local Ollama model. No Python or hosted media
service is required. Model interpretation is asynchronous, separate from
uploading and from downstream knowledge extraction.

## Local LFM3-Omni

For a text-only alternative, use `configs/lfm2.5-350m.example.json`.
The historical filename is retained for existing startup commands. It now selects
`qwen3.5:2b` with thinking disabled for text generation and
`embeddinggemma-2:440m` for 768-dimensional multilingual text embeddings. Media interpretation is disabled;
PDF/DOCX/text ingestion remains enabled (scanned PDFs require OCR tools).
It shares the existing SQLite database and blob directory, but uses a separate
EmbeddingGemma model ID. Existing Nomic/BGE vectors remain intact and cannot be reused
for EmbeddingGemma: explicitly re-embed existing memories after restarting.
The config retains lossless UTF-8 text chunking and pools
the chunk embeddings into one vector per memory without truncation. It uses a
separate model/strategy ID from earlier BGE configurations.
The text planner uses `extractive_only:true`: it selects source spans without
generating structured claims or graph links. Invalid selections are recorded as
`needs_review`; original storage and embeddings remain independent.
Restart server and workers with the new config; existing tenant planner
overrides may still take precedence. Pending media jobs require a capable
interpreter or explicit cancellation; changing config does not delete them.
Knowledge requests allow 20 minutes, with a 21-minute provider HTTP ceiling;
the worker budget is longer. The extractive prompt stays short and output is
limited to 256 tokens. Embedding settings and their model ID are unchanged.

Use the installed `lfm3-omni:latest` model with:

```sh
go run -tags devui ./cmd/niromem serve -dev-ui -mcp -mcp-user my-project -config configs/lfm3-omni.example.json
```

This config selects LFM3-Omni for both the text knowledge planner and media
interpretation, retains `nomic-embed-text` for text embeddings, and preserves the
same SQLite database and blob directory as the document example. It does not
silently replace the Qwen example or change tenant overrides; an existing tenant
planner override can still take precedence over the startup knowledge planner.
The example permits up to ten minutes for media interpretation and per knowledge
request for cold CPU model loading/extraction. These are background
deadlines, not a promise of
instant inference; upload acceptance does not wait for it.

Native `ffmpeg` and `ffprobe` are required for audio/video preprocessing. Install
them on each worker:

```sh
sudo apt-get install ffmpeg
```

Images need no conversion utility. The documents connector independently uses
Poppler/Tesseract for PDFs, not for LFM image interpretation.

## UI and pipeline

In **Remember -> Remember images, audio & video**, select files, set the
project/user scope and choose **Interpret media**. Uploads use two bounded lanes.
The existing leased worker performs interpretation; the UI is not locked while
the model runs. Use **Refresh media imports** after navigation/restart.

Supported inputs: PNG/JPEG, WAV/MP3/M4A/OGG, and MP4/WebM/MOV. Limits are
20 MiB per file, ten selected files per action, 16 million image pixels and
60 seconds per audio/video clip. The model context and output are bounded.
Video is sampled to at most eight frames and includes the bounded soundtrack
when present in the **same model request**. Sampling is not exhaustive viewing;
short events between frames can be missed. Silent video is not described as
having a soundtrack. Speech output generation is not included.

Original bytes are stored as typed image/audio/video episode parts in the
existing tenant-scoped blob storage. Upload records and durable interpretation
jobs commit together. The exact interpretation text and actual model identity
are persisted before source-linked chunks and follow-up jobs are committed.
Retries/restarts reuse that snapshot and cannot duplicate the import.

Descriptions/transcripts are explicitly **unreviewed model interpretations**,
not verified observations or guaranteed verbatim transcripts. They are stored
with low trust, original episode/blob references and model provenance. Normal
enrichment can extract entities, claims and relationships from this text under
the existing tenant mode. Disabled knowledge mode does not extract assertions;
shadow records suggestions; enforced adds unreviewed knowledge. None of these
modes makes the model's perception ground truth.

Search uses text descriptions/transcripts and their ordinary text embeddings,
not native image/audio embeddings. Original media remains downloadable through
the authenticated import inspector. Deleting an interpretation chunk does not
delete the retained original.

## Provider contract and API

`POST /v1/media` uses the existing API authentication and scope conventions:

```json
{"name":"photo.png","bytes":"BASE64_MEDIA_BYTES","user_id":"my-project"}
```

It returns HTTP 202 with import ID, queued status, selected model and an
`unreviewed` marker. The shared import APIs provide status, recent imports and
original download:

- `GET /v1/documents/{id}`
- `GET /v1/documents`
- `GET /v1/documents/{id}/file`

Media results include `kind: "media"`. The worker records explicit errors for
unsupported capabilities, missing tools, malformed media, excessive duration,
timeouts and incomplete/empty model responses. There is no text-only success
fallback when the attachment cannot be interpreted.

This Ollama build accepts image and WAV bytes in each message's `images`
attachment array. Other fields such as `audio` can be silently ignored by a
server. The connector uses the verified attachment path and checks advertised
model capabilities; merely having a model named “omni” is insufficient.

`multimodal.ollama` settings: `base_url`, `model`, `num_thread`, `num_ctx`,
`num_predict`, `timeout_ms`. Keep `worker.concurrency` at one on small CPUs;
parallel conversion/uploads do not justify unlimited model requests.

## Validation

Normal tests use generated fixtures and local HTTP test servers. An explicit
local-model smoke test sends only generated images, tones and short video:

```sh
NIROMEM_TEST_OMNI=1 go test -v ./cmd/niromem -run TestLocalOmniMedia -timeout 8m
```

It checks transport and non-empty interpretation, not the truth of arbitrary
model claims. Never treat successful inference as a factual accuracy guarantee.
